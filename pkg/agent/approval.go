package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// ApprovalHandler 定义工具执行前的审批入口（Human-in-the-loop）。
//
// 不同 Transport 实现不同的审批交互：CLI 在终端提示，Web 通过 WebSocket 弹窗。
type ApprovalHandler interface {
	// RequestApproval 请求对某次工具调用进行审批。
	//
	// 返回 approved=true 表示批准执行；false 表示拒绝。
	// err 非 nil 表示审批过程本身出错（如超时、连接断开）。
	RequestApproval(ctx context.Context, toolName string, arguments string) (approved bool, err error)
}

// CLIApprovalHandler 在终端标准输入上以 Y/N 提示进行审批。
type CLIApprovalHandler struct {
	// in 输入源，默认 os.Stdin（便于测试注入）。
	in *bufio.Reader
	// out 提示输出，默认 os.Stdout。
	out *os.File
	// mu 串行化终端交互，避免多个审批提示交错。
	mu sync.Mutex
}

// NewCLIApprovalHandler 创建一个基于终端的审批处理器。
func NewCLIApprovalHandler() *CLIApprovalHandler {
	return &CLIApprovalHandler{
		in:  bufio.NewReader(os.Stdin),
		out: os.Stdout,
	}
}

// RequestApproval 在终端展示待执行的工具与参数，等待用户输入 Y/N。
func (h *CLIApprovalHandler) RequestApproval(ctx context.Context, toolName string, arguments string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	fmt.Fprintf(h.out, "\n⚠️  工具调用需要审批\n  工具：%s\n  参数：%s\n是否允许执行？[y/N]: ", toolName, arguments)

	// 在独立 goroutine 中读取，以便同时响应 ctx 取消。
	// 注意：当 ctx 被取消后，goroutine 仍会阻塞在 ReadString 上，直到用户
	// 在终端输入一行或标准输入被关闭后才能退出。由于 channel 缓冲为 1，
	// goroutine 最终能写入并正常退出，不会永久泄漏。
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := h.in.ReadString('\n')
		ch <- result{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			// EOF（Ctrl+D）视为用户拒绝，输出提示后返回 false 而非错误，
			// 避免中断整个 Agent 循环——模型会收到"用户拒绝"的工具结果并自行决策。
			if errors.Is(r.err, io.EOF) {
				fmt.Fprintln(h.out, "\n[检测到 EOF，已自动拒绝工具执行]")
				return false, nil
			}
			return false, fmt.Errorf("读取审批输入失败：%w", r.err)
		}
		answer := strings.ToLower(strings.TrimSpace(r.line))
		return answer == "y" || answer == "yes", nil
	}
}

// ApprovalRequest 表示一次发往 Web 端的审批请求负载。
type ApprovalRequest struct {
	// ID 审批请求的唯一标识，用于将响应与请求关联。
	ID string `json:"id"`
	// ToolName 待执行的工具名称。
	ToolName string `json:"tool_name"`
	// Arguments 待执行的工具参数。
	Arguments string `json:"arguments"`
}

// ApprovalSender 抽象“将审批请求发送到远端”的能力（通常由 WebSocket 连接实现）。
type ApprovalSender func(req ApprovalRequest) error

// WSApprovalHandler 通过 WebSocket 发起审批请求并等待远端响应。
//
// 工作方式：
//   - RequestApproval 生成一个带唯一 ID 的审批请求，通过 send 回调下发；
//   - 为该 ID 创建一个等待 channel，并在 pending 中登记；
//   - Transport 层收到前端的审批响应后调用 Resolve(id, approved)，唤醒等待；
//   - 支持超时与 ctx 取消。
type WSApprovalHandler struct {
	// send 下发审批请求的回调。
	send ApprovalSender
	// timeout 等待前端响应的超时（<=0 表示不超时，仅受 ctx 约束）。
	timeout time.Duration
	// logger 用于记录晚到/无效的审批响应。
	logger *slog.Logger

	mu      sync.Mutex
	pending map[string]chan bool
	// seq 用于生成自增的请求 ID。
	seq uint64
}

// NewWSApprovalHandler 创建一个基于 WebSocket 的审批处理器。
func NewWSApprovalHandler(send ApprovalSender, timeout time.Duration) *WSApprovalHandler {
	return &WSApprovalHandler{
		send:    send,
		timeout: timeout,
		logger:  slog.Default(),
		pending: make(map[string]chan bool),
	}
}

// RequestApproval 下发审批请求并阻塞等待结果，直到收到响应、超时或 ctx 取消。
func (h *WSApprovalHandler) RequestApproval(ctx context.Context, toolName string, arguments string) (bool, error) {
	// 生成唯一 ID 并登记等待 channel。
	// 使用自增 seq 即可保证会话内唯一，无需额外拼接 UnixNano。
	h.mu.Lock()
	h.seq++
	id := fmt.Sprintf("approval-%d", h.seq)
	ch := make(chan bool, 1)
	h.pending[id] = ch
	h.mu.Unlock()

	// 确保退出时清理登记，避免泄漏。
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()

	// 下发请求。
	if err := h.send(ApprovalRequest{ID: id, ToolName: toolName, Arguments: arguments}); err != nil {
		return false, fmt.Errorf("发送审批请求失败：%w", err)
	}

	// 组装超时 channel（timeout<=0 时为 nil，永不触发）。
	var timeoutCh <-chan time.Time
	if h.timeout > 0 {
		timer := time.NewTimer(h.timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timeoutCh:
		return false, fmt.Errorf("等待审批超时（%s）", h.timeout)
	case approved := <-ch:
		return approved, nil
	}
}

// Resolve 由 Transport 层在收到前端审批响应时调用，唤醒对应的等待。
//
// 若该 id 不存在（已超时/已清理），记录日志并忽略。
// 若 channel 已满（极罕见：重复 Resolve），同样记录后忽略。
func (h *WSApprovalHandler) Resolve(id string, approved bool) {
	h.mu.Lock()
	ch, ok := h.pending[id]
	h.mu.Unlock()
	if !ok {
		// 超时后收到的晚到响应，记录以便排查审批流程问题。
		h.logger.Warn("收到未知或已超时的审批响应，已忽略", "approval_id", id)
		return
	}
	// 非阻塞写入（channel 缓冲为 1）。
	select {
	case ch <- approved:
	default:
		// channel 已满说明被重复 Resolve，记录异常。
		h.logger.Warn("审批收到重复响应，已忽略", "approval_id", id)
	}
}

// ArgumentAwareApproval 是工具可选实现的接口，允许工具根据实际调用参数
// 动态决定是否需要审批，而非统一返回固定值。
//
// 用途：CommandTool 的危险性取决于具体命令（ls 安全，rm -rf 危险），
// 实现此接口后，approvalTool 会在执行前先问工具是否真的需要审批，
// 只有危险命令才弹出审批提示，避免对所有命令都打扰用户。
type ArgumentAwareApproval interface {
	NeedApprovalFor(arguments string) bool
}

// approvalTool 是对 Tool 的装饰器：在执行前插入审批检查。
type approvalTool struct {
	// inner 被包装的原始工具。
	inner Tool
	// handler 审批处理器。
	handler ApprovalHandler
}

// WrapWithApproval 返回一个包装后的工具：若原工具需要审批，则在执行前先请求审批，
// 被拒绝时直接返回”用户拒绝”结果而不真正执行。
//
// 若原工具不需要审批（NeedApproval() 为 false），则直接返回原工具，避免额外开销。
func WrapWithApproval(tool Tool, handler ApprovalHandler) Tool {
	if tool == nil || handler == nil || !tool.NeedApproval() {
		return tool
	}
	return &approvalTool{inner: tool, handler: handler}
}

// Name 透传内部工具名称。
func (t *approvalTool) Name() string { return t.inner.Name() }

// Description 透传内部工具描述。
func (t *approvalTool) Description() string { return t.inner.Description() }

// Schema 透传内部工具定义。
func (t *approvalTool) Schema() provider.ToolDef { return t.inner.Schema() }

// NeedApproval 恒为 true（该装饰器仅在需要审批时创建）。
func (t *approvalTool) NeedApproval() bool { return true }

// Execute 先请求审批，批准后再执行内部工具；被拒绝或审批出错时返回相应结果。
//
// 若内部工具实现了 ArgumentAwareApproval，则会先询问它是否真正需要审批：
// 这样像 CommandTool 这类工具可以对安全命令（ls、pwd）直接放行，
// 只对危险命令（rm -rf、sudo）触发审批提示，减少不必要的打扰。
func (t *approvalTool) Execute(ctx context.Context, arguments string) (string, error) {
	// 若工具支持按参数动态决策，则先询问；无需审批时直接执行。
	if aa, ok := t.inner.(ArgumentAwareApproval); ok {
		if !aa.NeedApprovalFor(arguments) {
			return t.inner.Execute(ctx, arguments)
		}
	}

	approved, err := t.handler.RequestApproval(ctx, t.inner.Name(), arguments)
	if err != nil {
		return "", fmt.Errorf("审批失败：%w", err)
	}
	if !approved {
		// 以文本结果回填，交由模型决定后续策略（不视为执行错误）。
		return fmt.Sprintf("用户拒绝执行工具 %q。", t.inner.Name()), nil
	}
	return t.inner.Execute(ctx, arguments)
}
