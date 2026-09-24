// Package provider — Claude (Anthropic Messages API) 专用适配器。
//
// Anthropic 的 API 与 OpenAI 差异显著：
//   - 鉴权用 x-api-key 头（非 Authorization: Bearer）
//   - system prompt 不是 messages 数组的一部分，而是请求体顶层字段
//   - 工具调用的请求/响应格式不同（content blocks 而非 choices）
//   - 流式响应使用 message_start / content_block_start / content_block_delta /
//     content_block_stop / message_delta / message_stop 事件类型
//   - 必须显式传 max_tokens（无默认值）
//
// 本适配器通过嵌入 baseProvider 复用 HTTP 请求与重试逻辑。
//
// 参考文档: https://docs.anthropic.com/en/api/messages
package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	defaultClaudeBaseURL = "https://api.anthropic.com"
	defaultClaudeModel   = "claude-sonnet-4-20250514"
	// claudeAPIVersion Anthropic 要求在请求头中携带 API 版本
	claudeAPIVersion = "2023-06-01"
	defaultMaxTokens = 4096
)

// claudeProvider 实现 Anthropic Messages API 的 Provider。
// 嵌入 baseProvider 复用 HTTP 与重试逻辑。
type claudeProvider struct {
	baseProvider
}

// NewClaudeProvider 创建 Claude 专用 Provider。
func NewClaudeProvider(cfg ProviderConfig) (Provider, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultClaudeBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultClaudeModel
	}
	return &claudeProvider{
		baseProvider: baseProvider{
			name:         "claude",
			model:        model,
			baseURL:      baseURL,
			apiKey:       cfg.APIKey,
			extraHeaders: cfg.ExtraHeaders,
			httpClient:   newHTTPClient(),
		},
	}, nil
}

// ---- Anthropic 请求/响应结构体 ----

// claudeRequest Anthropic Messages API 请求体
type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []claudeMessage `json:"messages"`
	Tools     []claudeTool    `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	// 可选参数
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	StopSeqs    []string `json:"stop_sequences,omitempty"`
}

// claudeMessage Anthropic 消息格式
// 注意: Anthropic 消息的 content 可以是字符串或 content block 数组
type claudeMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string 或 []claudeContentBlock
}

// claudeContentBlock Anthropic 内容块
type claudeContentBlock struct {
	Type      string `json:"type"`                  // text / tool_use / tool_result
	Text      string `json:"text,omitempty"`        // type=text
	ID        string `json:"id,omitempty"`          // type=tool_use
	Name      string `json:"name,omitempty"`        // type=tool_use
	Input     any    `json:"input,omitempty"`       // type=tool_use, JSON object
	ToolUseID string `json:"tool_use_id,omitempty"` // type=tool_result
	Content   string `json:"content,omitempty"`     // type=tool_result (也可以是数组，这里简化为字符串)
	IsError   bool   `json:"is_error,omitempty"`    // type=tool_result
}

// claudeTool Anthropic 工具定义
type claudeTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// ---- 流式事件结构 ----

// claudeStreamEvent SSE 事件（Anthropic 用 event: 前缀标识事件类型）
type claudeStreamEvent struct {
	Type string `json:"type"`
	// message_start
	Message *claudeStreamMessage `json:"message,omitempty"`
	// content_block_start
	Index        int                 `json:"index,omitempty"`
	ContentBlock *claudeContentBlock `json:"content_block,omitempty"`
	// content_block_delta
	Delta *claudeStreamDelta `json:"delta,omitempty"`
	// message_delta
	Usage *claudeStreamUsage `json:"usage,omitempty"`
}

type claudeStreamMessage struct {
	ID    string             `json:"id"`
	Usage *claudeStreamUsage `json:"usage,omitempty"`
}

type claudeStreamDelta struct {
	Type        string `json:"type"`                   // text_delta / input_json_delta
	Text        string `json:"text,omitempty"`         // text_delta
	PartialJSON string `json:"partial_json,omitempty"` // input_json_delta
	StopReason  string `json:"stop_reason,omitempty"`  // message_delta 的 delta
}

type claudeStreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Chat 实现 Provider 接口，发起 Anthropic Messages API 流式请求。
func (p *claudeProvider) Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error) {
	o := applyOptions(opts...)

	// 分离 system prompt 和对话消息
	systemPrompt, convMessages := p.splitSystemPrompt(messages)

	maxTokens := defaultMaxTokens
	if o.MaxTokens != nil {
		maxTokens = *o.MaxTokens
	}

	reqBody := claudeRequest{
		Model:       p.model,
		MaxTokens:   maxTokens,
		System:      systemPrompt,
		Messages:    p.convertMessages(convMessages),
		Tools:       p.convertTools(tools),
		Stream:      true,
		Temperature: o.Temperature,
		TopP:        o.TopP,
		StopSeqs:    o.Stop,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("claude: 序列化请求体失败: %w", err)
	}

	// 使用 baseProvider.doWithRetry，Anthropic 鉴权通过 headerAnthropicKey
	url := p.baseURL + "/v1/messages"
	resp, err := p.doWithRetry(ctx, url, payload, headerAnthropicKey, func(req *http.Request) {
		req.Header.Set("anthropic-version", claudeAPIVersion)
	})
	if err != nil {
		return nil, err
	}

	out := make(chan StreamResponse)
	go p.streamResponse(ctx, resp, out)
	return out, nil
}

// splitSystemPrompt 将 system 消息从消息列表中分离出来
// Anthropic 要求 system prompt 作为请求体顶层字段
func (p *claudeProvider) splitSystemPrompt(messages []Message) (string, []Message) {
	var sb strings.Builder
	var rest []Message
	for _, m := range messages {
		if m.Role == RoleSystem {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(m.Content)
		} else {
			rest = append(rest, m)
		}
	}
	return sb.String(), rest
}

// convertMessages 将统一消息格式转换为 Anthropic 格式
func (p *claudeProvider) convertMessages(messages []Message) []claudeMessage {
	var result []claudeMessage

	for _, m := range messages {
		switch m.Role {
		case RoleUser:
			result = append(result, claudeMessage{Role: "user", Content: m.Content})

		case RoleAssistant:
			if len(m.ToolCalls) == 0 {
				// 纯文本回复
				result = append(result, claudeMessage{Role: "assistant", Content: m.Content})
			} else {
				// 包含工具调用的回复：转为 content blocks
				var blocks []claudeContentBlock
				if m.Content != "" {
					blocks = append(blocks, claudeContentBlock{Type: "text", Text: m.Content})
				}
				for _, tc := range m.ToolCalls {
					// 将 arguments JSON 字符串解析为 object
					var input any
					if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
						input = map[string]any{"raw": tc.Arguments}
					}
					blocks = append(blocks, claudeContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Name,
						Input: input,
					})
				}
				result = append(result, claudeMessage{Role: "assistant", Content: blocks})
			}

		case RoleTool:
			// Anthropic 要求同一轮的所有 tool_result 必须合并在同一个 user 消息中。
			// 若上一条已是"全部为 tool_result 块"的 user 消息，则追加到其中；否则新建。
			toolBlock := claudeContentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			}
			if n := len(result); n > 0 {
				if last := result[n-1]; last.Role == "user" {
					if blocks, ok := last.Content.([]claudeContentBlock); ok &&
						allToolResultBlocks(blocks) {
						result[n-1].Content = append(blocks, toolBlock)
						continue
					}
				}
			}
			result = append(result, claudeMessage{
				Role:    "user",
				Content: []claudeContentBlock{toolBlock},
			})
		}
	}

	return result
}

// allToolResultBlocks 判断给定 content blocks 是否全部为 tool_result 类型。
// 用于 convertMessages 中安全地将新 tool_result 合并到已有 user 消息，
// 避免将 text block + tool_result 混合的 user 消息错误地追加 tool_result 块。
func allToolResultBlocks(blocks []claudeContentBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// convertTools 将统一工具定义转换为 Anthropic 格式
func (p *claudeProvider) convertTools(tools []ToolDef) []claudeTool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]claudeTool, 0, len(tools))
	for _, t := range tools {
		result = append(result, claudeTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return result
}

// streamResponse 解析 Anthropic SSE 流
//
// Anthropic SSE 事件格式:
//
//	event: message_start        → 消息开始，含 message.id 和初始 usage
//	event: content_block_start  → 内容块开始（text 或 tool_use）
//	event: content_block_delta  → 内容块增量（text_delta 或 input_json_delta）
//	event: content_block_stop   → 内容块结束
//	event: message_delta        → 消息级增量（含 stop_reason 和最终 usage）
//	event: message_stop         → 消息结束
func (p *claudeProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	defer close(out)
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var currentEventType string

	// 累积工具调用：index -> {id, name, arguments}
	toolCalls := make(map[int]*ToolCall)
	var toolOrder []int

	// 跟踪 token 用量
	var inputTokens, outputTokens int

	// onEvent 更新当前事件类型（event: 前缀行）。
	onEvent := func(eventType string) {
		currentEventType = eventType
	}

	// handler 处理每条 data: 行，按 currentEventType 分发。
	// 返回 true 表示流应终止（message_stop）。
	handler := func(data string) (done bool) {
		var event claudeStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("claude: 解析事件失败: %w", err)})
			return false
		}

		switch currentEventType {
		case "message_start":
			// 记录初始 input tokens
			if event.Message != nil && event.Message.Usage != nil {
				inputTokens = event.Message.Usage.InputTokens
			}

		case "content_block_start":
			// 新内容块开始
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				tc := &ToolCall{
					ID:   event.ContentBlock.ID,
					Name: event.ContentBlock.Name,
				}
				toolCalls[event.Index] = tc
				toolOrder = append(toolOrder, event.Index)
			}

		case "content_block_delta":
			if event.Delta == nil {
				return false
			}
			switch event.Delta.Type {
			case "text_delta":
				// 文本增量
				if event.Delta.Text != "" {
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{Content: event.Delta.Text},
					})
				}
			case "input_json_delta":
				// 工具参数增量，累积到对应的 toolCall
				if tc, ok := toolCalls[event.Index]; ok {
					tc.Arguments += event.Delta.PartialJSON
				}
			}

		case "message_delta":
			// 消息级增量，含 stop_reason 和最终 usage
			if event.Delta != nil && event.Delta.StopReason != "" {
				finishReason := event.Delta.StopReason

				// Anthropic stop_reason: "end_turn" / "tool_use" / "stop_sequence" / "max_tokens"
				// 统一映射为 OpenAI 风格的 finish_reason
				var mappedReason string
				var completedToolCalls []ToolCall

				switch finishReason {
				case "tool_use":
					mappedReason = "tool_calls"
					// 输出累积的工具调用
					for _, idx := range toolOrder {
						if tc, ok := toolCalls[idx]; ok {
							completedToolCalls = append(completedToolCalls, *tc)
						}
					}
				case "end_turn":
					mappedReason = "stop"
				case "stop_sequence":
					mappedReason = "stop"
				case "max_tokens":
					mappedReason = "length"
				default:
					mappedReason = finishReason
				}

				sendResponse(ctx, out, StreamResponse{
					Delta: Delta{
						FinishReason: mappedReason,
						ToolCalls:    completedToolCalls,
					},
				})
			}
			// 最终 usage
			if event.Usage != nil {
				outputTokens = event.Usage.OutputTokens
			}

		case "message_stop":
			// 消息结束，发送 token 用量
			sendResponse(ctx, out, StreamResponse{
				Usage: &TokenUsage{
					PromptTokens:     inputTokens,
					CompletionTokens: outputTokens,
					TotalTokens:      inputTokens + outputTokens,
				},
			})
			return true
		}
		return false
	}

	onError := func(err error) {
		sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("claude: 读取流失败: %w", err)})
	}

	readSSELinesWithEvent(ctx, reader, onEvent, handler, onError)
}
