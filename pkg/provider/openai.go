package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// name 实例名称（通常等于配置中的 Type）。
	name string
	// model 使用的模型名称。
	model string
	// baseURL API 基础地址（不含末尾斜杠）。
	baseURL string
	// apiKey 访问密钥。
	apiKey string
	// extraHeaders 额外的请求头。
	extraHeaders map[string]string
	// httpClient 底层 HTTP 客户端。
	httpClient *http.Client
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
		name:         name,
		model:        model,
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		extraHeaders: cfg.ExtraHeaders,
		httpClient:   newHTTPClient(),
	}, nil
}

// Name 返回供应商实例名称。
func (p *openAIProvider) Name() string {
	return p.name
}

// Close 释放底层资源。对于标准 http.Client，仅关闭空闲连接。
func (p *openAIProvider) Close() error {
	p.httpClient.CloseIdleConnections()
	return nil
}

// ---- 请求 / 响应的 JSON 结构体（OpenAI Chat Completions 格式）----

// chatRequest 表示发送给 /chat/completions 的请求体。
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

// streamChunk 表示一条 SSE 数据行解析后的流式响应块。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

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
	resp, err := p.doWithRetry(ctx, payload)
	if err != nil {
		return nil, err
	}

	// 创建返回 channel，并在后台 goroutine 中解析流。
	out := make(chan StreamResponse)
	go p.streamResponse(ctx, resp, out)
	return out, nil
}

// doWithRetry 发送请求并在遇到可重试错误时按指数退避重试。
//
// 重试策略：
//   - 网络错误（连接失败、超时等）：重试；
//   - HTTP 5xx：重试；
//   - HTTP 4xx：不重试，直接返回错误；
//   - 2xx：成功返回响应。
func (p *openAIProvider) doWithRetry(ctx context.Context, payload []byte) (*http.Response, error) {
	url := p.baseURL + "/chat/completions"

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// 首次不等待；重试前使用 helpers.go 中的统一指数退避逻辑。
		if attempt > 0 {
			if !retrySleep(ctx, attempt) {
				return nil, ctx.Err()
			}
		}

		// 每次尝试都需要重新构造请求（body 是一次性读取的）。
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("构造请求失败: %w", err)
		}
		p.setHeaders(req)

		resp, err := p.httpClient.Do(req)
		if err != nil {
			// 网络层错误：如果是 context 取消则不再重试。
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("请求发送失败: %w", err)
			continue // 可重试
		}

		// 2xx：成功。
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		// 读取错误响应体用于诊断（readErrorBody 内部处理读取失败）。
		body := readErrorBody(resp)
		_ = resp.Body.Close()
		apiErr := parseAPIError(body)

		// 5xx：服务端错误，可重试。
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("服务端错误 (状态码 %d): %s", resp.StatusCode, apiErr)
			continue
		}

		// 4xx：客户端错误，不可重试，立即返回。
		return nil, fmt.Errorf("请求被拒绝 (状态码 %d): %s", resp.StatusCode, apiErr)
	}

	// 重试耗尽。
	return nil, fmt.Errorf("重试 %d 次后仍失败: %w", maxRetries, lastErr)
}

// setHeaders 设置请求头，包括鉴权、内容类型以及自定义头。
func (p *openAIProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	// 应用自定义请求头（可覆盖默认值）。
	for k, v := range p.extraHeaders {
		req.Header.Set(k, v)
	}
}

// streamResponse 解析 SSE 流，将增量结果写入 out channel，结束时关闭 channel。
//
// SSE 格式约定：
//   - 每行以 "data: " 开头，后接一段 JSON；
//   - 收到 "data: [DONE]" 表示流结束；
//   - 空行用于分隔事件，需忽略。
func (p *openAIProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	defer close(out)
	defer resp.Body.Close()

	// 工具调用需要跨多个分片累积（同一 index 的参数字符串会被拼接）。
	toolAccumulator := newToolCallAccumulator()

	reader := bufio.NewReader(resp.Body)
	for {
		// 优先响应 context 取消。
		if ctx.Err() != nil {
			sendResponse(ctx, out, StreamResponse{Err: ctx.Err()})
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// 正常结束（部分服务端可能不发送 [DONE]）。
				return
			}
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("读取流失败: %w", err)})
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue // 跳过空行（事件分隔符）。
		}

		// 仅处理 data 字段。
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		// 流结束标记。
		if data == "[DONE]" {
			return
		}

		// 解析单个数据块。
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 单块解析失败不应中断整个流，记录为错误并继续。
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("解析流数据块失败: %w", err)})
			continue
		}

		resp := buildStreamResponse(chunk, toolAccumulator)
		// 若该块没有任何有效负载（既无内容、无工具调用、无结束原因、无用量），则跳过。
		if resp.Delta.Content == "" && len(resp.Delta.ToolCalls) == 0 &&
			resp.Delta.FinishReason == "" && resp.Usage == nil {
			continue
		}

		if !sendResponse(ctx, out, resp) {
			return // context 已取消，停止发送。
		}
	}
}

// buildStreamResponse 将一个原始数据块转换为统一的 StreamResponse。
func buildStreamResponse(chunk streamChunk, acc *toolCallAccumulator) StreamResponse {
	var sr StreamResponse

	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		sr.Delta.Content = choice.Delta.Content
		sr.Delta.FinishReason = choice.FinishReason

		// 累积工具调用分片；当结束原因为 tool_calls 时输出完整结果。
		if len(choice.Delta.ToolCalls) > 0 {
			acc.add(choice.Delta.ToolCalls)
		}
		if choice.FinishReason == "tool_calls" {
			sr.Delta.ToolCalls = acc.result()
		}
	}

	// 用量统计（通常出现在最后一个块）。
	if chunk.Usage != nil {
		sr.Usage = &TokenUsage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
	}

	return sr
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
func parseAPIError(body []byte) string {
	if len(body) == 0 {
		return "(空响应体)"
	}
	var ae apiError
	if err := json.Unmarshal(body, &ae); err == nil && ae.Error.Message != "" {
		return ae.Error.Message
	}
	// 无法按结构解析时，直接返回原始文本。
	return string(body)
}

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
