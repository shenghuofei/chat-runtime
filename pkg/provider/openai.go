package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// openai.go 专属常量（maxRetries / baseRetryDelay / requestTimeout 已移至 helpers.go）。
const (
	// defaultOpenAIBaseURL OpenAI 官方 API 的默认基础地址。
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	// defaultModel 未在配置中指定模型时使用的兜底模型名。
	defaultModel = "gpt-3.5-turbo"
)

// openAIProvider 是基于 OpenAI Chat Completions API 的供应商实现。
//
// 由于 DeepSeek、Ollama、Ark（火山）、Qwen、OpenRouter 等大多数厂商
// 都提供了与 OpenAI 兼容的接口，因此它们可以直接复用本实现，
// 仅需在配置中指定不同的 BaseURL 与 Model。
type openAIProvider struct {
	baseProvider
}

// NewOpenAIProvider 根据配置创建一个 OpenAI 兼容的 Provider。
func NewOpenAIProvider(cfg ProviderConfig) (Provider, error) {
	// 归一化 BaseURL：为空则使用默认地址，并去除末尾斜杠。
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}

	// 归一化模型名。
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}

	// 供应商实例名兜底为 "openai"。
	name := cfg.Type
	if name == "" {
		name = "openai"
	}

	return &openAIProvider{
		baseProvider: baseProvider{
			name:         name,
			model:        model,
			baseURL:      baseURL,
			apiKey:       cfg.APIKey,
			extraHeaders: cfg.ExtraHeaders,
			httpClient:   newHTTPClient(),
		},
	}, nil
}

// Chat 发起一次流式对话请求。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	Stream      bool          `json:"stream"`
	StreamOpts  *streamOpts   `json:"stream_options,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
}

// streamOpts 控制流式响应的附加选项。
type streamOpts struct {
	// IncludeUsage 为 true 时，服务端会在流末尾额外返回 usage 统计。
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage 表示请求体中的一条消息。
type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// chatTool 表示请求体中的一个工具定义。
type chatTool struct {
	Type     string       `json:"type"` // 固定为 "function"
	Function chatFunction `json:"function"`
}

// chatFunction 表示工具的函数描述。
type chatFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// chatToolCall 表示消息 / 增量中的工具调用结构。
type chatToolCall struct {
	// Index 在流式响应中用于标识同一个工具调用的分片位置。
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function chatToolCallFunc `json:"function"`
}

// chatToolCallFunc 工具调用中的函数名与参数（参数为 JSON 字符串，可能分片）。
type chatToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// streamChunk 结构已移除：流式响应解析统一由 base.go 的 streamOpenAICompat
// 处理，其内部使用通用结构 openAICompatChunk。

// apiError 表示 API 返回的错误响应体。
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Chat 发起一次流式对话请求。
//
// 实现流程：
//  1. 将统一的 Message/ToolDef 转换为 OpenAI 请求格式；
//  2. 应用可选的生成参数；
//  3. 带重试地建立 HTTP 流式连接（网络错误 / 5xx 会退避重试，4xx 直接失败）；
//  4. 在独立 goroutine 中解析 SSE 流，将增量结果写入返回的 channel。
func (p *openAIProvider) Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error) {
	// 组合生成参数。
	o := applyOptions(opts...)

	// 构造请求体。
	reqBody := chatRequest{
		Model:       p.model,
		Messages:    convertMessages(messages),
		Tools:       convertTools(tools),
		Stream:      true,
		StreamOpts:  &streamOpts{IncludeUsage: true},
		Temperature: o.Temperature,
		MaxTokens:   o.MaxTokens,
		TopP:        o.TopP,
		Stop:        o.Stop,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	// 带重试地建立连接。
	url := p.baseURL + "/chat/completions"
	resp, err := p.doWithRetry(ctx, url, payload, headerBearer, nil)
	if err != nil {
		return nil, err
	}

	// 创建返回 channel，并在后台 goroutine 中解析流。
	out := make(chan StreamResponse)
	go p.streamResponse(ctx, resp, out)
	return out, nil
}

// streamResponse 解析 SSE 流，将增量结果写入 out channel，结束时关闭 channel。
//
// OpenAI 使用标准 OpenAI 兼容格式且不含 reasoning_content，
// 因此直接复用 base.go 的通用解析函数 streamOpenAICompat（hasReasoning=false）。
func (p *openAIProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	streamOpenAICompat(ctx, resp, out, openAICompatStreamConfig{providerName: p.name, hasReasoning: false})
}

// sendResponse 在遵守 context 取消的前提下向 channel 发送一个响应。
// 返回 false 表示 context 已取消、应停止发送。
func sendResponse(ctx context.Context, out chan<- StreamResponse, sr StreamResponse) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- sr:
		return true
	}
}

// ---- 类型转换辅助函数 ----

// convertMessages 将统一的 Message 列表转换为 OpenAI 请求格式。
func convertMessages(messages []Message) []chatMessage {
	result := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		cm := chatMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		// 转换 assistant 消息中的工具调用。
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: chatToolCallFunc{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		result = append(result, cm)
	}
	return result
}

// convertTools 将统一的 ToolDef 列表转换为 OpenAI 工具定义格式。
func convertTools(tools []ToolDef) []chatTool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]chatTool, 0, len(tools))
	for _, t := range tools {
		result = append(result, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return result
}

// parseAPIError 尝试从错误响应体中提取可读的错误信息。
// 已移至 base.go 的 parseAPIError 函数。

// ---- 工具调用累积器 ----

// toolCallAccumulator 用于在流式响应中累积分片的工具调用。
//
// OpenAI 的流式工具调用会将同一个调用拆成多个增量返回：
// 第一个分片包含 id/name，后续分片仅包含 arguments 的片段（需要拼接）。
// 通过 index 字段区分不同的工具调用。
type toolCallAccumulator struct {
	// order 记录 index 首次出现的顺序，保证输出顺序稳定。
	order []int
	// calls 以 index 为键累积各工具调用。
	calls map[int]*ToolCall
}

// newToolCallAccumulator 创建一个空的累积器。
func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		calls: make(map[int]*ToolCall),
	}
}

// add 将一批工具调用分片累积到内部状态中。
func (a *toolCallAccumulator) add(deltas []chatToolCall) {
	for _, d := range deltas {
		tc, ok := a.calls[d.Index]
		if !ok {
			tc = &ToolCall{}
			a.calls[d.Index] = tc
			a.order = append(a.order, d.Index)
		}
		// id / name 通常只在首个分片出现，遇到非空则赋值。
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Function.Name != "" {
			tc.Name = d.Function.Name
		}
		// 参数字符串按分片拼接。
		tc.Arguments += d.Function.Arguments
	}
}

// result 按 index 首次出现顺序返回累积完成的工具调用列表。
func (a *toolCallAccumulator) result() []ToolCall {
	result := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		result = append(result, *a.calls[idx])
	}
	return result
}
