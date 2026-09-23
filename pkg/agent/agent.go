// Package agent 是整个运行时的编排核心。
//
// 它驱动“LLM 调用 → 解析 tool_calls → （审批）执行工具 → 回填结果 → 再次调用 LLM”
// 的多轮 Tool Calling 循环，直到模型返回不含工具调用的终止响应。
//
// 本文件定义：
//   - Tool 接口：内置 / MCP / 自定义工具的统一抽象；
//   - 事件模型（Event / EventType / Usage）：向 Transport 层推送的流式事件；
//   - Agent 结构体与 Run 主循环。
//
// Session 生命周期见 session.go，工具审批装饰器见 approval.go。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/shenghuofei/chat-runtime/pkg/manager"
	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// EventType 标识一次流式事件的类型。
type EventType string

const (
	// EventToken 文本增量（LLM 流式输出的一个片段）。
	EventToken EventType = "token"
	// EventToolCall 模型请求调用某个工具。
	EventToolCall EventType = "tool_call"
	// EventToolResult 工具执行完成，回填结果。
	EventToolResult EventType = "tool_result"
	// EventApproval 工具执行前发起的审批请求。
	EventApproval EventType = "approval"
	// EventDone 本轮请求正常结束，携带用量统计。
	EventDone EventType = "done"
	// EventError 发生错误。
	EventError EventType = "error"
)

// Usage 描述一次请求的 token 用量统计。
type Usage struct {
	// PromptTokens 输入（prompt）消耗的 token 数。
	PromptTokens int `json:"prompt_tokens"`
	// CompletionTokens 输出（completion）生成的 token 数。
	CompletionTokens int `json:"completion_tokens"`
	// TotalTokens 总 token 数。
	TotalTokens int `json:"total_tokens"`
}

// Event 是 Agent 向 Transport 层推送的统一事件。
// 不同 Type 使用不同字段，未使用的字段留空。
type Event struct {
	// Type 事件类型。
	Type EventType `json:"type"`
	// Content 文本内容（EventToken / EventError 使用）。
	Content string `json:"content,omitempty"`
	// ToolName 工具名（EventToolCall / EventToolResult / EventApproval 使用）。
	ToolName string `json:"name,omitempty"`
	// ToolArgs 工具调用参数（EventToolCall / EventApproval 使用）。
	ToolArgs map[string]any `json:"args,omitempty"`
	// ToolResult 工具执行结果（EventToolResult 使用）。
	ToolResult string `json:"result,omitempty"`
	// ApprovalID 审批请求的唯一标识（EventApproval 使用），用于匹配审批回复。
	ApprovalID string `json:"approval_id,omitempty"`
	// Usage token 用量（EventDone 使用）。
	Usage *Usage `json:"usage,omitempty"`
}

// Message 表示一条历史消息，用于 History() 展示。
type Message struct {
	// Role 角色：system / user / assistant / tool。
	Role string `json:"role"`
	// Content 消息文本内容。
	Content string `json:"content"`
}

// Tool 是内置 / MCP / 自定义工具的统一抽象接口。
type Tool interface {
	// Name 返回工具名称（作为模型可调用的函数名）。
	Name() string
	// Description 返回工具用途说明。
	Description() string
	// Schema 返回供模型使用的工具定义（函数签名 + 参数 JSON Schema）。
	Schema() provider.ToolDef
	// NeedApproval 返回该工具在执行前是否需要人工审批。
	NeedApproval() bool
	// Execute 执行工具。arguments 为 JSON 字符串形式的参数，返回文本结果。
	Execute(ctx context.Context, arguments string) (string, error)
}

// AgentConfig 是 Agent 的运行时配置。
type AgentConfig struct {
	// MaxIterations Tool Calling 循环的最大迭代次数，防止无限循环。<=0 时使用默认值。
	MaxIterations int
	// OnMessageAdded 每条消息写入 manager 后触发，用于增量持久化。可为 nil。
	OnMessageAdded func(msg provider.Message)
}

// defaultMaxIterations 是 Tool Calling 循环的默认上限。
const defaultMaxIterations = 25

// Agent 编排一次会话的 LLM 调用与工具执行循环。
//
// Agent 不直接持有 system 提示词与历史，这些由注入的 Manager 统一维护，
// 从而与上下文溢出策略、持久化解耦。
type Agent struct {
	// provider LLM 适配器。
	provider provider.Provider
	// manager 上下文管理器（维护 system + 历史）。
	manager *manager.Manager
	// config 运行时配置。
	config AgentConfig

	// tools 已注册工具，key 为工具名。
	tools map[string]Tool
	// approval 审批处理器（可为 nil，表示不进行审批装饰）。
	approval ApprovalHandler

	// toolsMu 保护 tools 注册与 toolDefsCache 的并发访问。
	// 工具注册（RegisterTool）在 Run 之前完成，运行期只有读操作，
	// 但使用读写锁保证 race detector 也不会报错。
	toolsMu sync.RWMutex
	// toolDefsCache 工具定义列表的缓存，RegisterTool 时失效。
	// 工具列表在会话生命周期内几乎不变，无需每次 LLM 调用都重建。
	toolDefsCache []provider.ToolDef
	// toolDefsDirty 标记缓存是否需要重建。
	toolDefsDirty bool
}

// New 创建一个 Agent。
func New(p provider.Provider, mgr *manager.Manager, cfg AgentConfig) *Agent {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = defaultMaxIterations
	}
	return &Agent{
		provider:      p,
		manager:       mgr,
		config:        cfg,
		tools:         make(map[string]Tool),
		toolDefsDirty: true,
	}
}

// SetApprovalHandler 设置审批处理器。设置后，需要审批的工具会在执行前被装饰。
func (a *Agent) SetApprovalHandler(h ApprovalHandler) {
	a.approval = h
}

// RegisterTool 注册一个工具。若同名工具已存在则覆盖。
func (a *Agent) RegisterTool(t Tool) {
	if t == nil {
		return
	}
	a.toolsMu.Lock()
	a.tools[t.Name()] = t
	a.toolDefsDirty = true // 使缓存失效
	a.toolsMu.Unlock()
}

// ToolNames 返回已注册工具的名称列表（已排序，保证 Prompt Cache 前缀稳定）。
func (a *Agent) ToolNames() []string {
	a.toolsMu.RLock()
	defer a.toolsMu.RUnlock()
	names := make([]string, 0, len(a.tools))
	for name := range a.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// History 返回当前会话历史（不含 system），供展示使用。
func (a *Agent) History() []Message {
	raw := a.manager.Raw()
	out := make([]Message, 0, len(raw))
	for _, m := range raw {
		out = append(out, Message{Role: string(m.Role), Content: m.Content})
	}
	return out
}

// Clear 清空会话历史（保留 system 提示词）。
func (a *Agent) Clear() {
	a.manager.Load(nil)
}

// toolDefs 返回按名称排序的工具定义列表（稳定顺序有利于 Prompt Cache 命中）。
//
// 结果会被缓存：工具列表在会话生命周期内几乎不变，只有 RegisterTool 后才重建，
// 避免每次 LLM 调用都分配新切片并重排序。读写锁保证并发安全。
func (a *Agent) toolDefs() []provider.ToolDef {
	// 快速路径：缓存有效时只需读锁。
	a.toolsMu.RLock()
	if !a.toolDefsDirty && a.toolDefsCache != nil {
		defs := a.toolDefsCache
		a.toolsMu.RUnlock()
		return defs
	}
	a.toolsMu.RUnlock()

	// 缓存失效：升级为写锁并重建。
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	// 二次检查：另一个 goroutine 可能已经重建。
	if !a.toolDefsDirty && a.toolDefsCache != nil {
		return a.toolDefsCache
	}
	names := make([]string, 0, len(a.tools))
	for name := range a.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]provider.ToolDef, 0, len(names))
	for _, name := range names {
		defs = append(defs, a.tools[name].Schema())
	}
	a.toolDefsCache = defs
	a.toolDefsDirty = false
	return defs
}

// resolveTool 返回工具，若需要审批且已配置审批处理器，则返回审批装饰后的工具。
//
// 注意：WrapWithApproval 内部已判断 NeedApproval()，不需要审批的工具会直接返回原始实例。
func (a *Agent) resolveTool(name string) (Tool, bool) {
	a.toolsMu.RLock()
	t, ok := a.tools[name]
	a.toolsMu.RUnlock()
	if !ok {
		return nil, false
	}
	if a.approval != nil {
		return WrapWithApproval(t, a.approval), true
	}
	return t, true
}

// ToolCount 返回已注册工具数量。
func (a *Agent) ToolCount() int {
	a.toolsMu.RLock()
	defer a.toolsMu.RUnlock()
	return len(a.tools)
}

// Run 执行一次完整的对话请求，通过 emit 流式推送事件。
//
// 流程：追加 user 消息 → 循环调用 LLM → 若含 tool_calls 则执行工具并回填 → 再次调用，
// 直到返回不含 tool_calls 的终止响应或达到最大迭代次数。ctx 取消会中断循环。
func (a *Agent) Run(ctx context.Context, input string, emit func(Event)) error {
	if emit == nil {
		emit = func(Event) {}
	}

	// 追加用户输入。
	a.manager.AddUserMessage(provider.Message{Content: input})
	if a.config.OnMessageAdded != nil {
		a.config.OnMessageAdded(provider.Message{Role: provider.RoleUser, Content: input})
	}

	var total Usage

	for iter := 0; iter < a.config.MaxIterations; iter++ {
		// 检查取消。
		if err := ctx.Err(); err != nil {
			return err
		}

		messages := a.manager.GetMessages()
		stream, err := a.provider.Chat(ctx, messages, a.toolDefs())
		if err != nil {
			emit(Event{Type: EventError, Content: err.Error()})
			return err
		}

		// 消费流式响应，累积文本与工具调用。
		var contentBuf strings.Builder
		var toolCalls []provider.ToolCall
		var streamErr error
		for chunk := range stream {
			if chunk.Err != nil {
				streamErr = chunk.Err
				for range stream {
				}
				break
			}
			if chunk.Delta.Content != "" {
				contentBuf.WriteString(chunk.Delta.Content)
				emit(Event{Type: EventToken, Content: chunk.Delta.Content})
			}
			if len(chunk.Delta.ToolCalls) > 0 {
				toolCalls = append(toolCalls, chunk.Delta.ToolCalls...)
			}
			if chunk.Usage != nil {
				total.PromptTokens += chunk.Usage.PromptTokens
				total.CompletionTokens += chunk.Usage.CompletionTokens
				total.TotalTokens += chunk.Usage.TotalTokens
				a.manager.ReportUsage(*chunk.Usage)
			}
		}
		if streamErr != nil {
			emit(Event{Type: EventError, Content: streamErr.Error()})
			return streamErr
		}
		content := contentBuf.String()

		// 记录本轮 assistant 消息（可能携带 tool_calls）。
		assistantMsg := provider.Message{
			Content:   content,
			ToolCalls: toolCalls,
		}
		a.manager.AddAssistantMessage(assistantMsg)
		if a.config.OnMessageAdded != nil {
			assistantMsg.Role = provider.RoleAssistant
			a.config.OnMessageAdded(assistantMsg)
		}

		// 无工具调用：本轮结束。
		if len(toolCalls) == 0 {
			emit(Event{Type: EventDone, Usage: &total})
			return nil
		}

		// 执行每个工具调用并回填结果。
		// 同一轮的多个 tool_calls 并行执行以提高效率。
		type toolResult struct {
			callID string
			name   string
			result string
		}
		results := make([]toolResult, len(toolCalls))
		var wg sync.WaitGroup
		for i, tc := range toolCalls {
			if err := ctx.Err(); err != nil {
				return err
			}
			wg.Add(1)
			go func(idx int, call provider.ToolCall) {
				defer wg.Done()
				// 捕获任何 panic，避免单个工具的崩溃导致 wg.Wait 永久阻塞。
				defer func() {
					if r := recover(); r != nil {
						errMsg := fmt.Sprintf("工具 %q 执行时发生 panic: %v", call.Name, r)
						emit(Event{Type: EventError, Content: errMsg})
						results[idx] = toolResult{
							callID: call.ID,
							name:   call.Name,
							result: errMsg,
						}
					}
				}()
				res := a.execToolCall(ctx, call, emit)
				results[idx] = toolResult{callID: call.ID, name: call.Name, result: res}
			}(i, tc)
		}
		wg.Wait()

		// 按原始顺序回填结果（保持 Prompt Cache 稳定性）。
		for _, tr := range results {
			a.manager.AddToolResult(tr.callID, tr.name, tr.result)
			if a.config.OnMessageAdded != nil {
				a.config.OnMessageAdded(provider.Message{
					Role:       provider.RoleTool,
					ToolCallID: tr.callID,
					Name:       tr.name,
					Content:    tr.result,
				})
			}
		}
		// 继续下一轮循环，让模型基于工具结果继续生成。
	}

	// 达到最大迭代次数仍未终止。
	err := fmt.Errorf("已达到最大工具调用迭代次数 (%d)", a.config.MaxIterations)
	emit(Event{Type: EventError, Content: err.Error()})
	return err
}

// execToolCall 执行单个工具调用，推送相关事件并返回结果文本。
func (a *Agent) execToolCall(ctx context.Context, tc provider.ToolCall, emit func(Event)) string {
	// 解析参数用于事件展示（失败不阻断执行）。
	args := parseArgs(tc.Arguments)
	emit(Event{Type: EventToolCall, ToolName: tc.Name, ToolArgs: args})

	tool, ok := a.resolveTool(tc.Name)
	if !ok {
		result := fmt.Sprintf("未找到工具 %q。", tc.Name)
		emit(Event{Type: EventToolResult, ToolName: tc.Name, ToolResult: result})
		return result
	}

	result, err := tool.Execute(ctx, tc.Arguments)
	if err != nil {
		result = fmt.Sprintf("工具执行失败：%v", err)
	}
	emit(Event{Type: EventToolResult, ToolName: tc.Name, ToolResult: result})
	return result
}

// parseArgs 将 JSON 字符串参数解析为 map（解析失败时返回原始字符串包装）。
func parseArgs(arguments string) map[string]any {
	if arguments == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return map[string]any{"_raw": arguments}
	}
	return m
}
