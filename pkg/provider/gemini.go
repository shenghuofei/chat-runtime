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
// 参考文档: https://ai.google.dev/api/generate-content
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

const (
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
	defaultGeminiModel   = "gemini-2.0-flash"
)

// geminiProvider 实现 Google Gemini API 的 Provider。
type geminiProvider struct {
	name         string
	model        string
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	httpClient   *http.Client
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
		name:         "gemini",
		model:        model,
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		extraHeaders: cfg.ExtraHeaders,
		httpClient:   newHTTPClient(),
	}, nil
}

func (p *geminiProvider) Name() string { return p.name }

func (p *geminiProvider) Close() error {
	p.httpClient.CloseIdleConnections()
	return nil
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

	resp, err := p.doWithRetry(ctx, payload)
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
				var args map[string]any
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

// doWithRetry 带重试的 HTTP 请求
func (p *geminiProvider) doWithRetry(ctx context.Context, payload []byte) (*http.Response, error) {
	// Gemini 流式端点: streamGenerateContent?alt=sse&key=xxx
	url := fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s",
		p.baseURL, p.model, p.apiKey)

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if !retrySleep(ctx, attempt) {
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("gemini: 构造请求失败: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		for k, v := range p.extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := p.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("gemini: 请求发送失败: %w", err)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		body := readErrorBody(resp)
		_ = resp.Body.Close()

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = fmt.Errorf("gemini: 服务端错误 (状态码 %d): %s", resp.StatusCode, truncateBody(body))
			continue
		}

		return nil, fmt.Errorf("gemini: 请求被拒绝 (状态码 %d): %s", resp.StatusCode, truncateBody(body))
	}

	return nil, fmt.Errorf("gemini: 重试 %d 次后仍失败: %w", maxRetries, lastErr)
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
	// 用于生成稳定的 tool_call ID
	toolCallCounter := 0

	for {
		if ctx.Err() != nil {
			sendResponse(ctx, out, StreamResponse{Err: ctx.Err()})
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return
			}
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("gemini: 读取流失败: %w", err)})
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
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk geminiStreamResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("gemini: 解析数据块失败: %w", err)})
			continue
		}

		// 处理 candidates
		for _, candidate := range chunk.Candidates {
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{Content: part.Text},
					})
				}
				if part.FunctionCall != nil {
					// Gemini 的 functionCall 是完整的（非流式分片），直接输出
					argsJSON, _ := json.Marshal(part.FunctionCall.Args)
					toolCallCounter++
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{
							ToolCalls: []ToolCall{{
								ID:        fmt.Sprintf("call_%d", toolCallCounter),
								Name:      part.FunctionCall.Name,
								Arguments: string(argsJSON),
							}},
							FinishReason: "tool_calls",
						},
					})
				}
			}

			// 处理 finishReason
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
				// 仅在非 tool_calls 时发送（tool_calls 已在 functionCall 分支中发送）
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
	}
}
