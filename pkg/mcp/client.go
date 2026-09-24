// Package mcp 实现 MCP（Model Context Protocol）客户端封装。
//
// 它包装 mark3labs/mcp-go 的客户端，负责：
//   - 按配置（stdio / sse / streamable-http）建立连接并初始化；
//   - 发现 MCP Server 暴露的工具，并按 include/exclude 过滤；
//   - 将每个 MCP 工具适配为 agent.Tool 接口，工具名统一加 `<server>__` 前缀；
//   - 依据 auto_approve 决定工具是否需要审批；
//   - 提供 Server 级 / 工具级的并发串行化控制。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/provider"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// toolNameSeparator 是 Server 名与工具名之间的分隔符（双下划线）。
const toolNameSeparator = "__"

// clientInfo 是初始化握手时上报的客户端信息。
var clientInfo = mcp.Implementation{
	Name:    "chat-runtime",
	Version: "1.0.0",
}

// MCPClient 封装一个到 MCP Server 的连接。
type MCPClient struct {
	// config 该 Server 的配置。
	config config.MCPServerConfig
	// raw 底层 mcp-go 客户端。
	raw *mcpclient.Client
	// rawMu 保护 raw 字段的并发读写：reconnect 会替换 raw（写），
	// 而 callTool / DiscoverTools / Close 会读取 raw，需用读写锁避免数据竞争。
	rawMu sync.RWMutex

	// serverMu Server 级串行锁：no_concurrent=true 时，所有工具调用共用它。
	serverMu sync.Mutex
	// toolMu 工具级串行锁：为 no_concurrent_tools 中的每个工具各分配一把锁。
	toolMu map[string]*sync.Mutex
	// noConcurrent 是否整体串行。
	noConcurrent bool
	// reconnectMu 防止并发重连。
	reconnectMu sync.Mutex
	// closed 标记客户端已被关闭，防止 callTool 在 Close 后触发不必要的重连。
	closed atomic.Bool
}

// NewMCPClient 依据配置创建并初始化一个 MCP 客户端。
//
// 支持三种传输方式：
//   - stdio：启动本地子进程，通过 stdin/stdout 通信；
//   - sse：连接远程 SSE 服务；
//   - streamable-http：连接远程可流式 HTTP 服务。
//
// ctx 用于控制初始化超时与取消，调用方应传入带有合理 deadline 的 context。
func NewMCPClient(ctx context.Context, cfg config.MCPServerConfig) (*MCPClient, error) {
	// Server 名称不能包含工具名分隔符（双下划线），否则 "serverA__serverB__tool" 无法
	// 唯一确定属于哪个 Server，导致工具名歧义。
	if strings.Contains(cfg.Name, toolNameSeparator) {
		return nil, fmt.Errorf("MCP Server 名称 %q 不能包含 %q，请使用不含双下划线的名称", cfg.Name, toolNameSeparator)
	}

	raw, err := newRawClient(cfg)
	if err != nil {
		return nil, err
	}

	// 建立连接（stdio 在构造时已启动；SSE/HTTP 需显式 Start）。
	if err := raw.Start(ctx); err != nil {
		// stdio 的部分实现会在构造时自动 Start，此处失败一般可忽略；
		// 但为稳妥，非 stdio 情况下的失败应上报。
		if cfg.Transport != "stdio" {
			return nil, fmt.Errorf("启动 MCP 客户端失败：%w", err)
		}
	}

	// 初始化握手。
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = clientInfo
	if _, err := raw.Initialize(ctx, initReq); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("初始化 MCP 会话失败：%w", err)
	}

	// 预分配工具级锁。
	toolMu := make(map[string]*sync.Mutex, len(cfg.NoConcurrentTools))
	for _, name := range cfg.NoConcurrentTools {
		toolMu[name] = &sync.Mutex{}
	}

	return &MCPClient{
		config:       cfg,
		raw:          raw,
		toolMu:       toolMu,
		noConcurrent: cfg.NoConcurrent,
	}, nil
}

// newRawClient 按传输方式创建底层 mcp-go 客户端。
func newRawClient(cfg config.MCPServerConfig) (*mcpclient.Client, error) {
	switch cfg.Transport {
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio 传输缺少 command")
		}
		env := mapToEnvSlice(cfg.Env)
		return mcpclient.NewStdioMCPClient(cfg.Command, env, cfg.Args...)

	case "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("sse 传输缺少 url")
		}
		var opts []transport.ClientOption
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(cfg.Headers))
		}
		return mcpclient.NewSSEMCPClient(cfg.URL, opts...)

	case "streamable-http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("streamable-http 传输缺少 url")
		}
		var opts []transport.StreamableHTTPCOption
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(cfg.Headers))
		}
		return mcpclient.NewStreamableHttpClient(cfg.URL, opts...)

	default:
		return nil, fmt.Errorf("不支持的 MCP 传输方式：%q", cfg.Transport)
	}
}

// mapToEnvSlice 将 map 形式的环境变量转为 "K=V" 切片。
func mapToEnvSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// DiscoverTools 向 Server 发起工具发现，按 include/exclude 过滤后，
// 将每个工具适配为 agent.Tool（带 `<server>__` 前缀）返回。
//
// ctx 用于控制请求超时与取消。
func (c *MCPClient) DiscoverTools(ctx context.Context) ([]agent.Tool, error) {
	c.rawMu.RLock()
	raw := c.raw
	c.rawMu.RUnlock()
	res, err := raw.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("发现 MCP 工具失败：%w", err)
	}

	tools := make([]agent.Tool, 0, len(res.Tools))
	for _, mt := range res.Tools {
		// 过滤基于原始工具名（不含前缀）。
		if !c.shouldInclude(mt.Name) {
			continue
		}
		t := c.newMCPTool(mt)
		if t == nil {
			// 工具原始名含分隔符（__），无法唯一解析，跳过。
			slog.Warn("跳过工具名含分隔符的 MCP 工具，可能引起歧义",
				"server", c.config.Name,
				"tool", mt.Name,
				"separator", toolNameSeparator,
			)
			continue
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// shouldInclude 依据 include/exclude 判断某原始工具名是否应暴露。
//
// 优先级：先应用 include（若配置了白名单，则仅白名单内的工具通过），
// 再应用 exclude（从中剔除黑名单工具）。
func (c *MCPClient) shouldInclude(rawName string) bool {
	if len(c.config.Include) > 0 && !slices.Contains(c.config.Include, rawName) {
		return false
	}
	if slices.Contains(c.config.Exclude, rawName) {
		return false
	}
	return true
}

// isAutoApproved 依据 auto_approve 策略判断某原始工具名是否自动批准。
//
//   - All=true：该 Server 所有工具自动批准；
//   - Tools 列表：仅列表内工具自动批准；
//   - 否则：需要审批。
func (c *MCPClient) isAutoApproved(rawName string) bool {
	aa := c.config.AutoApprove
	if aa.All {
		return true
	}
	return slices.Contains(aa.Tools, rawName)
}

// prefixedName 返回带 Server 前缀的工具名：`<server>__<tool>`。
func (c *MCPClient) prefixedName(rawName string) string {
	return c.config.Name + toolNameSeparator + rawName
}

// lockFor 返回某原始工具应使用的串行锁；无需串行时返回 nil。
//
//   - noConcurrent=true：返回 Server 级锁（所有工具共用，整体串行）；
//   - 否则若该工具在 no_concurrent_tools 中：返回其工具级锁；
//   - 否则：返回 nil（可并发）。
func (c *MCPClient) lockFor(rawName string) *sync.Mutex {
	if c.noConcurrent {
		return &c.serverMu
	}
	if mu, ok := c.toolMu[rawName]; ok {
		return mu
	}
	return nil
}

// callTool 调用一次远端工具并将结果内容拼为文本返回。
func (c *MCPClient) callTool(ctx context.Context, rawName string, arguments string) (string, error) {
	// 快速失败：客户端已关闭，不再尝试调用或重连。
	if c.closed.Load() {
		return "", fmt.Errorf("调用 MCP 工具 %q 失败：客户端已关闭", rawName)
	}

	// 解析参数（模型给出 JSON 字符串）。
	var argMap map[string]any
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &argMap); err != nil {
			return "", fmt.Errorf("解析工具参数失败：%w", err)
		}
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = rawName
	req.Params.Arguments = argMap

	c.rawMu.RLock()
	raw := c.raw
	c.rawMu.RUnlock()
	res, err := raw.CallTool(ctx, req)
	if err != nil {
		// 工具调用失败时尝试重连一次（对 stdio 子进程崩溃场景尤为有效）。
		// 短暂退避后再重连，给 MCP Server 重启留出缓冲时间，
		// 避免在服务端重启瞬间发起密集连接风暴。
		// 使用 NewTimer+Stop 而非 time.After，确保 ctx 提前取消时定时器被释放，
		// 不在后台泄漏 goroutine 直到 500ms 耗尽。
		backoff := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			backoff.Stop()
			return "", fmt.Errorf("调用 MCP 工具 %q 失败（上下文已取消）：%w", rawName, err)
		case <-backoff.C:
		}
		backoff.Stop() // 若已触发本调用为空操作，保持安全
		// Close 后不重连：若客户端已被关闭（如会话清理），直接返回错误。
		if c.closed.Load() {
			return "", fmt.Errorf("调用 MCP 工具 %q 失败（客户端已关闭）：%w", rawName, err)
		}
		// 传入当时读取的 raw，若已被其他 goroutine 重连替换则 reconnect 直接返回。
		if rerr := c.reconnect(ctx, raw); rerr != nil {
			return "", fmt.Errorf("调用 MCP 工具 %q 失败且重连失败：%w；重连错误：%w", rawName, err, rerr)
		}
		// reconnect 已替换 c.raw（或已被其他 goroutine 替换），重新读取最新的客户端。
		c.rawMu.RLock()
		raw = c.raw
		c.rawMu.RUnlock()
		res, err = raw.CallTool(ctx, req)
		if err != nil {
			return "", fmt.Errorf("MCP 工具 %q 重连后调用仍失败：%w", rawName, err)
		}
	}

	text := extractText(res)
	if res.IsError {
		return text, fmt.Errorf("MCP 工具 %q 返回错误：%s", rawName, text)
	}
	return text, nil
}

// reconnect 关闭当前连接并重新建立（含 Start + Initialize），加锁防止并发重连。
// failedRaw 为调用方发现失败时持有的 raw 指针：若当前 c.raw 已不是它，
// 说明已有其他 goroutine 完成重连，本次直接返回避免重复重连。
func (c *MCPClient) reconnect(ctx context.Context, failedRaw *mcpclient.Client) error {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	c.rawMu.RLock()
	oldRaw := c.raw
	c.rawMu.RUnlock()
	// 幂等检查：若 c.raw 已被其他 goroutine 替换，则本次无需重连。
	if oldRaw != failedRaw {
		return nil
	}
	if oldRaw != nil {
		_ = oldRaw.Close()
	}

	raw, err := newRawClient(c.config)
	if err != nil {
		return fmt.Errorf("重建 MCP 客户端失败：%w", err)
	}

	if err := raw.Start(ctx); err != nil {
		if c.config.Transport != "stdio" {
			return fmt.Errorf("重启 MCP 客户端失败：%w", err)
		}
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = clientInfo
	if _, err := raw.Initialize(ctx, initReq); err != nil {
		_ = raw.Close()
		return fmt.Errorf("重新初始化 MCP 会话失败：%w", err)
	}

	c.rawMu.Lock()
	c.raw = raw
	c.rawMu.Unlock()
	return nil
}

// extractText 从 CallToolResult 中提取所有文本内容并拼接。
func extractText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// Close 关闭底层客户端连接（对 stdio 会终止子进程）。
//
// 先置位 closed 标记，再关闭底层连接，保证并发的 callTool 在连接关闭后
// 不会再触发重连（reconnect 会看到 closed=true 并跳过重连）。
func (c *MCPClient) Close() error {
	c.closed.Store(true)
	c.rawMu.RLock()
	raw := c.raw
	c.rawMu.RUnlock()
	if raw == nil {
		return nil
	}
	return raw.Close()
}

// MCPTool 将一个 MCP 工具适配为 agent.Tool 接口。
type MCPTool struct {
	// client 所属的 MCP 客户端（用于回调 callTool 与并发控制）。
	client *MCPClient
	// rawName 原始工具名（不含前缀）。
	rawName string
	// name 带前缀的工具名。
	name string
	// description 工具描述。
	description string
	// schema 工具定义（含参数 Schema）。
	schema provider.ToolDef
	// autoApprove 是否自动批准（true 表示无需审批）。
	autoApprove bool
}

// 编译期断言：MCPTool 实现 agent.Tool。
var _ agent.Tool = (*MCPTool)(nil)

// newMCPTool 由发现到的 mcp.Tool 构造一个 MCPTool。
//
// 若工具原始名包含分隔符（双下划线），则拼接后的工具名无法唯一解析属于哪个 Server，
// 跳过并打印 Warn 日志，避免工具名歧义。
func (c *MCPClient) newMCPTool(mt mcp.Tool) *MCPTool {
	if strings.Contains(mt.Name, toolNameSeparator) {
		return nil
	}
	return &MCPTool{
		client:      c,
		rawName:     mt.Name,
		name:        c.prefixedName(mt.Name),
		description: mt.Description,
		schema: provider.ToolDef{
			Name:        c.prefixedName(mt.Name),
			Description: mt.Description,
			Parameters:  inputSchemaToMap(mt.InputSchema),
		},
		autoApprove: c.isAutoApproved(mt.Name),
	}
}

// Name 返回带前缀的工具名。
func (t *MCPTool) Name() string { return t.name }

// Description 返回工具描述。
func (t *MCPTool) Description() string { return t.description }

// Schema 返回工具定义。
func (t *MCPTool) Schema() provider.ToolDef { return t.schema }

// NeedApproval 返回是否需要审批（自动批准时为 false）。
func (t *MCPTool) NeedApproval() bool { return !t.autoApprove }

// Execute 执行该 MCP 工具，按需施加 Server/工具级串行控制。
func (t *MCPTool) Execute(ctx context.Context, arguments string) (string, error) {
	if mu := t.client.lockFor(t.rawName); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return t.client.callTool(ctx, t.rawName, arguments)
}

// inputSchemaToMap 将 mcp 的 ToolInputSchema 转为通用 map（JSON Schema）。
func inputSchemaToMap(s mcp.ToolInputSchema) map[string]interface{} {
	m := map[string]interface{}{
		"type": s.Type,
	}
	if s.Properties != nil {
		m["properties"] = s.Properties
	} else {
		m["properties"] = map[string]interface{}{}
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	return m
}
