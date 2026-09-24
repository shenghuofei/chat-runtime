// Package provider — Gemini (Google AI) 专用适配器。
//
// Gemini 的 API 与 OpenAI 差异显著：
//   - 使用 generateContent / streamGenerateContent 端点
//   - 鉴权通过 URL query 参数 ?key=xxx 或 Authorization: Bearer
//   - 消息角色为 "user" / "model"（非 "assistant"）
//   - system instruction 是请求体顶层字段（非 messages 的一部分）
//   - 工具调用使用 functionCall / functionResponse 而非 tool_calls
//   - 流式响应格式完全不同（JSON 数组或 SSE，取决于端点）
//
// 本适配器通过嵌入 baseProvider 复用 HTTP 请求与重试逻辑。
//
// 参考文档: https://ai.google.dev/api/generate-content
package provider

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
	defaultGeminiModel   = "gemini-2.0-flash"
)

// geminiProvider 实现 Google Gemini API 的 Provider。
// 嵌入 baseProvider 复用 HTTP 与重试逻辑。
type geminiProvider struct {
	baseProvider
}

// NewGeminiProvider 创建 Gemini 专用 Provider。
func NewGeminiProvider(cfg ProviderConfig) (Provider, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultGeminiModel
	}
	return &geminiProvider{
		baseProvider: baseProvider{
			name:         "gemini",
			model:        model,
			baseURL:      baseURL,
			apiKey:       cfg.APIKey,
			extraHeaders: cfg.ExtraHeaders,
			httpClient:   newHTTPClient(),
		},
	}, nil
}

// ---- Gemini 请求/响应结构体 ----

// geminiRequest Gemini generateContent 请求体
type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiToolDeclaration `json:"tools,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

// geminiContent 对应一轮对话
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart 消息内容的一个部分
type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResponse `json:"functionResponse,omitempty"`
}

// geminiFunctionCall 模型发起的函数调用
type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// geminiFuncResponse 函数执行结果
type geminiFuncResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// geminiToolDeclaration 工具声明
type geminiToolDeclaration struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations,omitempty"`
}

// geminiFuncDecl 函数声明
type geminiFuncDecl struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// geminiGenerationConfig 生成配置
type geminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// ---- 流式响应结构 ----

// geminiStreamResponse Gemini 流式响应块
type geminiStreamResponse struct {
	Candidates    []geminiCandidate    `json:"candidates"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// Chat 实现 Provider 接口
func (p *geminiProvider) Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error) {
	o := applyOptions(opts...)

	systemInstruction, contents := p.convertMessages(messages)

	reqBody := geminiRequest{
		Contents:          contents,
		SystemInstruction: systemInstruction,
		Tools:             p.convertTools(tools),
	}

	// 生成配置
	genConfig := &geminiGenerationConfig{
		Temperature:   o.Temperature,
		TopP:          o.TopP,
		StopSequences: o.Stop,
	}
	if o.MaxTokens != nil {
		genConfig.MaxOutputTokens = o.MaxTokens
	}
	reqBody.GenerationConfig = genConfig

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: 序列化请求体失败: %w", err)
	}

	// Gemini 鉴权通过 x-goog-api-key 请求头传递 API Key（由 headerGoogleKey 在 setHeaders 中设置），
	// 不再将 key 明文拼入 URL query，避免其出现在 URL / 日志 / 代理记录中。
	// 无需额外的 extraSetup 回调重复设置同一请求头。
	url := fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse",
		p.baseURL, p.model)
	resp, err := p.doWithRetry(ctx, url, payload, headerGoogleKey, nil)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamResponse)
	go p.streamResponse(ctx, resp, out)
	return out, nil
}

// convertMessages 将统一消息格式转换为 Gemini 格式
// Gemini 使用 "user" / "model" 角色，system 作为顶层字段
func (p *geminiProvider) convertMessages(messages []Message) (*geminiContent, []geminiContent) {
	var systemInstruction *geminiContent
	var contents []geminiContent

	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			// Gemini 的 systemInstruction 是单独字段
			if systemInstruction == nil {
				systemInstruction = &geminiContent{
					Parts: []geminiPart{{Text: m.Content}},
				}
			} else {
				systemInstruction.Parts = append(systemInstruction.Parts, geminiPart{Text: m.Content})
			}

		case RoleUser:
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})

		case RoleAssistant:
			var parts []geminiPart
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			// 工具调用转为 functionCall parts
			for _, tc := range m.ToolCalls {
				args := make(map[string]any)
				if tc.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Arguments), &args)
				}
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tc.Name,
						Args: args,
					},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, geminiContent{
					Role:  "model",
					Parts: parts,
				})
			}

		case RoleTool:
			// 工具结果转为 functionResponse（属于 user 角色的 turn）。
			// Gemini API 要求：同一轮的多个工具结果必须合并到同一个 user 消息的多个
			// parts 中，而不是各自独立为一条 user 消息，否则 API 会报格式错误。
			var respData map[string]any
			if err := json.Unmarshal([]byte(m.Content), &respData); err != nil {
				respData = map[string]any{"result": m.Content}
			}
			part := geminiPart{
				FunctionResponse: &geminiFuncResponse{
					Name:     m.Name,
					Response: respData,
				},
			}
			// 若上一条内容已是工具结果组（user 角色且首个 part 为 FunctionResponse），
			// 则追加到同一消息；否则新建一条 user 消息。
			if len(contents) > 0 &&
				contents[len(contents)-1].Role == "user" &&
				len(contents[len(contents)-1].Parts) > 0 &&
				contents[len(contents)-1].Parts[0].FunctionResponse != nil {
				contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, part)
			} else {
				contents = append(contents, geminiContent{
					Role:  "user",
					Parts: []geminiPart{part},
				})
			}
		}
	}

	return systemInstruction, contents
}

// convertTools 转换工具定义
func (p *geminiProvider) convertTools(tools []ToolDef) []geminiToolDeclaration {
	if len(tools) == 0 {
		return nil
	}
	var decls []geminiFuncDecl
	for _, t := range tools {
		decls = append(decls, geminiFuncDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return []geminiToolDeclaration{{FunctionDeclarations: decls}}
}

// streamResponse 解析 Gemini SSE 流
//
// Gemini SSE 格式（alt=sse 模式下）:
//
//	data: {"candidates":[...], "usageMetadata":{...}}
//
// 每个 data 行是一个完整的 JSON 对象，包含一个或多个 candidate。
// candidate 的 content.parts 中可能包含 text 或 functionCall。
func (p *geminiProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	defer close(out)
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	// 用于生成唯一的 tool_call ID。
	// 使用 crypto/rand 生成前缀，防止同一会话多轮调用产生重复 ID（如每轮都有 call_1）。
	// 避免使用 time.Now().UnixNano()：在某些平台（如 Windows）时钟精度仅毫秒级，
	// 同一毫秒内多次调用会产生相同前缀，导致 ID 碰撞。
	var randBuf [8]byte
	if _, err := cryptorand.Read(randBuf[:]); err != nil {
		// crypto/rand 极少失败；退化为确定性占位符（每次调用内 counter 仍保证唯一）。
		copy(randBuf[:], []byte("fallback"))
	}
	idPrefix := "gc_" + hex.EncodeToString(randBuf[:])
	toolCallCounter := 0

	readSSELines(ctx, reader, func(data string) bool {
		if data == "[DONE]" {
			return true
		}

		var chunk geminiStreamResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("gemini: 解析数据块失败: %w", err)})
			return false
		}

		// 处理 candidates
		for _, candidate := range chunk.Candidates {
			// 先收集当前 candidate 所有 parts 中的文本与工具调用，
			// 再统一发送工具调用事件（附带 FinishReason）。
			// 避免每个 functionCall part 各自发送一次 FinishReason:"tool_calls"
			// 导致上层循环多次触发工具执行逻辑。
			var candidateToolCalls []ToolCall
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{Content: part.Text},
					})
				}
				if part.FunctionCall != nil {
					// Gemini 的 functionCall 是完整的（非流式分片），累积后统一输出。
					argsJSON, _ := json.Marshal(part.FunctionCall.Args)
					toolCallCounter++
					candidateToolCalls = append(candidateToolCalls, ToolCall{
						ID:        fmt.Sprintf("%s_%d", idPrefix, toolCallCounter),
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
					})
				}
			}
			// 若有工具调用，合并为一个事件，FinishReason 仅发送一次。
			if len(candidateToolCalls) > 0 {
				sendResponse(ctx, out, StreamResponse{
					Delta: Delta{
						ToolCalls:    candidateToolCalls,
						FinishReason: "tool_calls",
					},
				})
			}

			// 处理 finishReason（非 tool_calls 情况下单独发送）
			if candidate.FinishReason != "" {
				var mappedReason string
				switch candidate.FinishReason {
				case "STOP":
					mappedReason = "stop"
				case "MAX_TOKENS":
					mappedReason = "length"
				case "SAFETY":
					mappedReason = "content_filter"
				default:
					mappedReason = strings.ToLower(candidate.FinishReason)
				}
				// tool_calls 情况下 FinishReason 已在上方随工具调用列表一并发送。
				if mappedReason != "" && mappedReason != "tool_calls" {
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{FinishReason: mappedReason},
					})
				}
			}
		}

		// 处理 usage
		if chunk.UsageMetadata != nil {
			sendResponse(ctx, out, StreamResponse{
				Usage: &TokenUsage{
					PromptTokens:     chunk.UsageMetadata.PromptTokenCount,
					CompletionTokens: chunk.UsageMetadata.CandidatesTokenCount,
					TotalTokens:      chunk.UsageMetadata.TotalTokenCount,
				},
			})
		}
		return false
	}, func(err error) {
		sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("gemini: 读取流失败: %w", err)})
	})
}
