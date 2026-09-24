// Package provider — 各专用 Provider 共享的 HTTP 请求与流式解析基础设施。
//
// baseProvider 封装了可复用的：
//   - HTTP 客户端与请求头设置；
//   - 带指数退避的重试逻辑（doWithRetry）；
//   - SSE 流读取的基础循环（readSSELines）；
//   - reasoning_content（思维链）的状态机处理。
//
// 各具体 Provider 通过组合（embedding）baseProvider 来复用这些能力，
// 仅需覆写差异部分（请求构造、特殊字段解析等）。
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

// baseProvider 是各专用 Provider 的通用基础结构。
type baseProvider struct {
	// name 供应商实例名称。
	name string
	// model 使用的模型名称。
	model string
	// baseURL API 基础地址（不含末尾斜杠）。
	baseURL string
	// apiKey 访问密钥。
	apiKey string
	// extraHeaders 额外的自定义请求头。
	extraHeaders map[string]string
	// httpClient 底层 HTTP 客户端。
	httpClient *http.Client
}

// Name 返回供应商实例名称。
func (b *baseProvider) Name() string { return b.name }

// Close 释放底层 HTTP 资源。
func (b *baseProvider) Close() error {
	b.httpClient.CloseIdleConnections()
	return nil
}

// headerStyle 控制请求头的鉴权方式。
type headerStyle int

const (
	// headerBearer 使用 Authorization: Bearer <key> 鉴权（OpenAI / DeepSeek / Ark 等）。
	headerBearer headerStyle = iota
	// headerAnthropicKey 使用 x-api-key 鉴权（Claude / Anthropic）。
	headerAnthropicKey
	// headerNone 不设置鉴权头（保留用于无需鉴权的场景）。
	headerNone
	// headerGoogleKey 使用 x-goog-api-key 请求头鉴权（Gemini / Google AI）。
	// 相比拼入 URL query 参数，请求头方式可避免 API Key 出现在 URL、日志、代理记录中。
	headerGoogleKey
)

// setHeaders 设置请求头，包括鉴权、内容类型以及自定义头。
func (b *baseProvider) setHeaders(req *http.Request, style headerStyle) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	switch style {
	case headerBearer:
		if b.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+b.apiKey)
		}
	case headerAnthropicKey:
		if b.apiKey != "" {
			req.Header.Set("x-api-key", b.apiKey)
		}
	case headerNone:
		// 不设置鉴权头
	case headerGoogleKey:
		// Gemini 使用 x-goog-api-key 请求头传递 API Key，避免明文拼入 URL。
		if b.apiKey != "" {
			req.Header.Set("x-goog-api-key", b.apiKey)
		}
	}
	for k, v := range b.extraHeaders {
		req.Header.Set(k, v)
	}
}

// doWithRetry 发送请求并在遇到可重试错误时按指数退避重试。
//
// 参数：
//   - ctx: 请求上下文
//   - url: 完整请求 URL
//   - payload: 请求体
//   - style: 鉴权方式
//   - extraSetup: 可选的额外请求设置回调（如 Claude 需设置 anthropic-version）
//
// 重试策略：
//   - 网络错误 / HTTP 5xx / 429：重试；
//   - HTTP 4xx（非 429）：不重试；
//   - 2xx：成功。
func (b *baseProvider) doWithRetry(ctx context.Context, url string, payload []byte, style headerStyle, extraSetup func(*http.Request)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if !retrySleep(ctx, attempt) {
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("%s: 构造请求失败: %w", b.name, err)
		}
		b.setHeaders(req, style)
		if extraSetup != nil {
			extraSetup(req)
		}

		resp, err := b.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				// 保留网络错误细节，同时包装 context 错误以便调用方用 errors.Is 判断取消/超时。
				return nil, fmt.Errorf("%s: 请求被中断（%w）: %v", b.name, ctx.Err(), err)
			}
			lastErr = fmt.Errorf("%s: 请求发送失败: %w", b.name, err)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		body := readErrorBody(resp)
		_ = resp.Body.Close()
		errMsg := parseAPIError(body)

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = fmt.Errorf("%s: 服务端错误 (状态码 %d): %s", b.name, resp.StatusCode, errMsg)
			continue
		}

		return nil, fmt.Errorf("%s: 请求被拒绝 (状态码 %d): %s", b.name, resp.StatusCode, errMsg)
	}

	return nil, fmt.Errorf("%s: 重试 %d 次后仍失败: %w", b.name, maxRetries, lastErr)
}

// SSELineHandler 是处理单条 SSE data 行的回调函数。
// 返回 true 表示流应终止（已收到 [DONE] 或最终事件）。
type SSELineHandler func(data string) (done bool)

// readSSELines 读取 SSE 流并将 data 行逐条回调给 handler。
//
// 处理逻辑：
//   - 跳过空行（事件分隔符）；
//   - 仅处理 "data:" 前缀的行；
//   - 遇到 "data: [DONE]" 或 handler 返回 true 时终止；
//   - IO 错误通过 onError 回调上报。
func readSSELines(ctx context.Context, reader *bufio.Reader, handler SSELineHandler, onError func(error)) {
	for {
		if ctx.Err() != nil {
			onError(ctx.Err())
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return
			}
			onError(err)
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			handler(data) // 让 handler 有机会做收尾
			return
		}

		if handler(data) {
			return
		}
	}
}

// readSSELinesWithEvent 读取带 event: 前缀的 SSE 流（如 Anthropic Messages API）。
//
// 与 readSSELines 相比，本函数额外处理 "event:" 前缀行：每读到一行 event:
// 就通过 onEvent 回调告知当前事件类型，随后的 data: 行由 handler 处理。
//
// 处理逻辑：
//   - 跳过空行（事件分隔符）；
//   - "event:" 行 → 调用 onEvent(eventType)；
//   - "data:" 行 → 调用 handler(data)，handler 返回 true 或遇到 EOF/错误时终止；
//   - 忽略空的 data 行；
//   - IO 错误通过 onError 回调上报。
//
// 注意：Anthropic 的流以 message_stop 事件（由 handler 判定）结束，
// 通常不会发送 "data: [DONE]"，因此本函数不对 [DONE] 做特殊处理。
func readSSELinesWithEvent(ctx context.Context, reader *bufio.Reader, onEvent func(eventType string), handler SSELineHandler, onError func(error)) {
	for {
		if ctx.Err() != nil {
			onError(ctx.Err())
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return
			}
			onError(err)
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// event: 前缀行 —— 更新当前事件类型。
		if strings.HasPrefix(line, "event:") {
			eventType := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			onEvent(eventType)
			continue
		}

		// data: 前缀行 —— 交由 handler 处理。
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}

		if handler(data) {
			return
		}
	}
}

// reasoningTracker 跟踪 reasoning_content（思维链）的状态，
// 在进入/退出思维链时自动插入 <think> / </think> 标签。
//
// DeepSeek 和 Ark 的 reasoning_content 处理逻辑完全相同，通过此结构复用。
type reasoningTracker struct {
	inReasoning bool
}

// handleReasoning 处理一个流式增量块中的 reasoning_content 和 content。
// 返回需要发送的文本事件列表（可能为空、一条或多条）。
func (rt *reasoningTracker) handleReasoning(reasoningContent, content string) []string {
	var events []string

	if reasoningContent != "" {
		if !rt.inReasoning {
			rt.inReasoning = true
			events = append(events, "<think>\n")
		}
		events = append(events, reasoningContent)
	}

	if content != "" {
		if rt.inReasoning {
			rt.inReasoning = false
			events = append(events, "</think>\n\n")
		}
		events = append(events, content)
	}

	return events
}

// closeIfNeeded 在流结束时，如果仍在 reasoning 中则补上关闭标签。
// 返回需要发送的文本（可能为空字符串）。
func (rt *reasoningTracker) closeIfNeeded() string {
	if rt.inReasoning {
		rt.inReasoning = false
		return "</think>\n\n"
	}
	return ""
}

// ---- OpenAI 兼容格式的通用流解析 ----

// openAICompatChunk 是 OpenAI 兼容格式 SSE 数据块的统一结构（取各 Provider 的超集）。
//
// 该结构合并了 OpenAI / DeepSeek / Ark 的差异字段：
//   - Delta.ReasoningContent：DeepSeek / Ark 的思维链输出（OpenAI 无此字段，反序列化时保持零值）；
//   - Usage：三者共有的 token 用量统计。
//
// 各 Provider 特有的扩展字段（如 DeepSeek 的 cache 统计、Ark 的 prompt_tokens_details）
// 不在通用结构中体现——它们不影响 StreamResponse 的产出，故统一忽略。
type openAICompatChunk struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// openAICompatStreamConfig 配置 OpenAI 兼容格式的 SSE 流解析。
type openAICompatStreamConfig struct {
	providerName string // 错误前缀（如 "deepseek"、"ark"、"openai"）
	hasReasoning bool   // 是否支持 reasoning_content（DeepSeek / Ark 为 true）
}

// streamOpenAICompat 解析 OpenAI 兼容格式的 SSE 流（含可选 reasoning）。
//
// 内部复用 readSSELines 读取 SSE、toolCallAccumulator 累积分片工具调用；
// 当 cfg.hasReasoning=true 时，使用 reasoningTracker 处理 reasoning_content
// 与正常内容之间的 <think>/</think> 标签切换。
//
// 该函数负责关闭 out channel 与响应体，调用方无需再处理。
func streamOpenAICompat(ctx context.Context, resp *http.Response, out chan<- StreamResponse, cfg openAICompatStreamConfig) {
	defer close(out)
	defer resp.Body.Close()

	toolAccumulator := newToolCallAccumulator()
	reader := bufio.NewReader(resp.Body)

	// 仅在支持 reasoning 时创建跟踪器；否则为 nil，跳过相关处理。
	var rt *reasoningTracker
	if cfg.hasReasoning {
		rt = &reasoningTracker{}
	}

	readSSELines(ctx, reader, func(data string) bool {
		if data == "[DONE]" {
			// 流结束：若仍处于 reasoning 中，补上关闭标签。
			if rt != nil {
				if closing := rt.closeIfNeeded(); closing != "" {
					sendResponse(ctx, out, StreamResponse{Delta: Delta{Content: closing}})
				}
			}
			return true
		}

		var chunk openAICompatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 单块解析失败不中断整个流，记录为错误并继续。
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("%s: 解析数据块失败: %w", cfg.providerName, err)})
			return false
		}

		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]

			if rt != nil {
				// 支持 reasoning：通过 reasoningTracker 处理思维链/正常内容切换。
				for _, text := range rt.handleReasoning(choice.Delta.ReasoningContent, choice.Delta.Content) {
					sendResponse(ctx, out, StreamResponse{Delta: Delta{Content: text}})
				}
			} else if choice.Delta.Content != "" {
				// 不支持 reasoning：直接输出内容。
				sendResponse(ctx, out, StreamResponse{Delta: Delta{Content: choice.Delta.Content}})
			}

			// 工具调用累积。
			if len(choice.Delta.ToolCalls) > 0 {
				toolAccumulator.add(choice.Delta.ToolCalls)
			}

			// 结束原因。
			if choice.FinishReason != "" {
				if rt != nil {
					if closing := rt.closeIfNeeded(); closing != "" {
						sendResponse(ctx, out, StreamResponse{Delta: Delta{Content: closing}})
					}
				}
				sr := StreamResponse{Delta: Delta{FinishReason: choice.FinishReason}}
				if choice.FinishReason == "tool_calls" {
					sr.Delta.ToolCalls = toolAccumulator.result()
				}
				sendResponse(ctx, out, sr)
			}
		}

		// Token 用量（通常出现在最后一个块）。
		if chunk.Usage != nil {
			sendResponse(ctx, out, StreamResponse{
				Usage: &TokenUsage{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
					TotalTokens:      chunk.Usage.TotalTokens,
				},
			})
		}
		return false
	}, func(err error) {
		sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("%s: 读取流失败: %w", cfg.providerName, err)})
	})
}

// buildOpenAICompatRequest 构造 OpenAI 兼容格式请求体的公共部分，
// 供 openAIProvider / deepSeekProvider / arkProvider 等复用，消除重复代码。
//
// 调用方在返回前可按需调整字段（如推理模型置空 Temperature / TopP，
// 或将 MaxTokens 替换为 MaxCompletionTokens）。
func buildOpenAICompatRequest(model string, messages []Message, tools []ToolDef, o options) chatRequest {
	return chatRequest{
		Model:       model,
		Messages:    convertMessages(messages),
		Tools:       convertTools(tools),
		Stream:      true,
		StreamOpts:  &streamOpts{IncludeUsage: true},
		Temperature: o.Temperature,
		MaxTokens:   o.MaxTokens,
		TopP:        o.TopP,
		Stop:        o.Stop,
	}
}

// parseAPIError 尝试从错误响应体中提取可读的错误信息。
//
// 多数 LLM Provider 的错误响应遵循 {"error":{"message":"..."}} 格式，
// 本函数尝试从中提取 message 字段；提取失败则退化为 truncateBody。
func parseAPIError(body []byte) string {
	if len(body) == 0 {
		return "(空响应体)"
	}
	// 尝试按 OpenAI 风格 {"error":{"message":"..."}} 解析。
	var ae struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &ae); err == nil && ae.Error.Message != "" {
		return ae.Error.Message
	}
	// 无法按结构解析时，返回截断后的原始文本。
	return truncateBody(body)
}
