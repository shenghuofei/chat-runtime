// Package provider 定义了对接各类 LLM Provider 的统一抽象类型。
//
// 本文件集中定义跨层复用的核心数据结构（消息、工具调用、Token 用量、
// 流式响应、生成参数选项等）。Manager（上下文管理）、Store（会话持久化）、
// Config（配置解析）以及各具体 Provider 实现均依赖这里的类型。
package provider

import "context"

// Role 表示一条消息的角色。
type Role string

const (
	// RoleSystem 系统提示词，位于消息序列最前，作为最稳定的 Prompt Cache 前缀。
	RoleSystem Role = "system"
	// RoleUser 用户消息。
	RoleUser Role = "user"
	// RoleAssistant 助手（模型）消息，可能包含 tool_calls。
	RoleAssistant Role = "assistant"
	// RoleTool 工具执行结果消息，需与 assistant 的 tool_calls 一一对应。
	RoleTool Role = "tool"
)

// ToolCall 表示模型发起的一次工具调用请求。
//
// 采用扁平结构（ID / Name / Arguments），便于各 Provider 直接读写，
// 避免多层嵌套带来的转换成本。
type ToolCall struct {
	// ID 是本次工具调用的唯一标识，工具结果通过该 ID 回填。
	ID string `json:"id"`
	// Type 通常为 "function"，预留以兼容不同 Provider 的扩展类型。
	Type string `json:"type,omitempty"`
	// Name 被调用的工具/函数名称。
	Name string `json:"name"`
	// Arguments 工具参数，通常为 JSON 字符串（流式场景下可能分片拼接而成）。
	Arguments string `json:"arguments"`
}

// Message 表示对话中的一条消息，是 Provider / Manager / Store 三层共享的核心结构。
type Message struct {
	// Role 消息角色：system / user / assistant / tool。
	Role Role `json:"role"`
	// Content 文本内容。对于纯 tool_calls 的 assistant 消息，可能为空。
	Content string `json:"content"`
	// ToolCalls 仅 assistant 消息可能包含，表示模型请求调用的工具列表。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 仅 tool 消息使用，标识本结果对应的 tool_call.ID。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name 仅 tool 消息使用，记录产生该结果的工具名称，便于审计与标准化。
	Name string `json:"name,omitempty"`
}

// ToolDef 描述一个可供模型调用的工具定义（函数签名）。
type ToolDef struct {
	// Name 工具名称。
	Name string `json:"name"`
	// Description 工具用途说明，供模型理解何时调用。
	Description string `json:"description,omitempty"`
	// Parameters 工具参数的 JSON Schema。
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

// TokenUsage 记录一次或累计的 Token 用量，用于 window 模式的上下文预算控制与计费统计。
type TokenUsage struct {
	// PromptTokens 输入（prompt）消耗的 token 数。
	PromptTokens int `json:"prompt_tokens"`
	// CompletionTokens 输出（completion）消耗的 token 数。
	CompletionTokens int `json:"completion_tokens"`
	// TotalTokens 总 token 数，通常等于 PromptTokens + CompletionTokens。
	TotalTokens int `json:"total_tokens"`
}

// Add 累加另一份 Token 用量并返回累加后的结果。
func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     u.PromptTokens + other.PromptTokens,
		CompletionTokens: u.CompletionTokens + other.CompletionTokens,
		TotalTokens:      u.TotalTokens + other.TotalTokens,
	}
}

// StreamResponse 表示流式对话中的一个增量响应块。
type StreamResponse struct {
	// Delta 本块的增量内容（文本片段 / 工具调用 / 结束原因）。
	Delta Delta
	// Usage Token 用量统计，通常仅在流末尾出现，其余块为 nil。
	Usage *TokenUsage
	// Err 本块处理过程中遇到的错误；非 nil 时表示该块无效。
	Err error
}

// Delta 表示一个流式增量的具体内容。
type Delta struct {
	// Content 文本增量片段。
	Content string
	// ToolCalls 累积完成的工具调用（通常在 FinishReason 为 tool_calls 时给出）。
	ToolCalls []ToolCall
	// FinishReason 结束原因，如 "stop" / "tool_calls"，未结束时为空。
	FinishReason string
}

// ProviderConfig 定义创建一个 Provider 实例所需的连接配置。
type ProviderConfig struct {
	// Type Provider 类型：openai / claude / deepseek / ollama / ark / gemini / qwen。
	Type string
	// APIKey 访问密钥。
	APIKey string
	// BaseURL 自定义 API endpoint（代理 / 私有部署 / 兼容网关）。
	BaseURL string
	// Model 使用的模型名称。
	Model string
	// ExtraHeaders 额外的自定义请求头。
	ExtraHeaders map[string]string
	// IsReasoner 显式声明该模型为推理模型（如 deepseek-reasoner、o1 等）。
	// 推理模型通常不支持 temperature/top_p，且可能使用不同的 max_tokens 字段名。
	// 若为 false，Provider 会根据模型名做启发式判断（如检查名称是否包含 "reasoner"）。
	IsReasoner bool
}

// Provider 是各 LLM 供应商的统一抽象接口。
type Provider interface {
	// Name 返回供应商实例名称。
	Name() string
	// Chat 发起一次流式对话请求，返回增量响应的只读 channel。
	Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error)
	// Close 释放底层资源。
	Close() error
}

// options 汇总一次请求的可选生成参数。
type options struct {
	// Temperature 采样温度。
	Temperature *float64
	// MaxTokens 最大生成 token 数。
	MaxTokens *int
	// TopP 核采样参数。
	TopP *float64
	// Stop 停止序列。
	Stop []string
}

// Option 是配置请求生成参数的函数式选项。
type Option func(*options)

// applyOptions 依次应用所有选项并返回合并后的配置。
func applyOptions(opts ...Option) options {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithTemperature 设置采样温度。
func WithTemperature(t float64) Option {
	return func(o *options) { o.Temperature = &t }
}

// WithMaxTokens 设置最大生成 token 数。
func WithMaxTokens(n int) Option {
	return func(o *options) { o.MaxTokens = &n }
}

// WithTopP 设置核采样参数。
func WithTopP(p float64) Option {
	return func(o *options) { o.TopP = &p }
}

// WithStop 追加停止序列。
func WithStop(stop ...string) Option {
	return func(o *options) { o.Stop = append(o.Stop, stop...) }
}
