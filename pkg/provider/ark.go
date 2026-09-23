// Package provider — 火山引擎方舟 (Ark) 专用适配器。
//
// 火山引擎方舟（Volcengine Ark）是字节跳动的 LLM 服务平台，
// 托管了豆包（Doubao）等模型。其 API 基于 OpenAI 兼容格式，但有以下差异：
//   - 模型名使用 endpoint ID（如 ep-20241208120114-7csw8），而非模型名
//   - 支持 reasoning_content（豆包 thinking 模式）
//   - 支持联网搜索（web_search）参数
//   - Token 用量包含 prompt_tokens_details（cached_tokens）
//   - 鉴权使用标准 Bearer token
//
// 参考文档: https://www.volcengine.com/docs/82379/1298454
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
	defaultArkBaseURL = "https://ark.cn-beijing.volces.com/api/v3"
	defaultArkModel   = "ep-20241208120114-7csw8" // 需要用户替换为实际的 endpoint ID
)

// arkProvider 实现火山引擎方舟 API 的 Provider。
type arkProvider struct {
	name         string
	model        string // 实际是 endpoint ID
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	httpClient   *http.Client
}

// NewArkProvider 创建火山引擎方舟专用 Provider。
func NewArkProvider(cfg ProviderConfig) (Provider, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultArkBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultArkModel
	}
	return &arkProvider{
		name:         "ark",
		model:        model,
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		extraHeaders: cfg.ExtraHeaders,
		httpClient:   &http.Client{Timeout: requestTimeout},
	}, nil
}

func (p *arkProvider) Name() string { return p.name }

func (p *arkProvider) Close() error {
	p.httpClient.CloseIdleConnections()
	return nil
}

// ---- Ark 特有的响应结构 ----

// arkStreamChunk 扩展 OpenAI 格式，增加火山引擎特有字段
type arkStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"` // 豆包 thinking 输出
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *arkUsage `json:"usage"`
}

// arkUsage 火山引擎扩展的 token 用量
type arkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// 火山引擎特有: prompt cache 统计
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// arkRequest 火山引擎请求体，扩展 OpenAI 格式
type arkRequest struct {
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

// Chat 实现 Provider 接口。
func (p *arkProvider) Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error) {
	o := applyOptions(opts...)

	reqBody := arkRequest{
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
		return nil, fmt.Errorf("ark: 序列化请求体失败: %w", err)
	}

	resp, err := p.doWithRetry(ctx, payload)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamResponse)
	go p.streamResponse(ctx, resp, out)
	return out, nil
}

// doWithRetry 带重试的 HTTP 请求
func (p *arkProvider) doWithRetry(ctx context.Context, payload []byte) (*http.Response, error) {
	url := p.baseURL + "/chat/completions"

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if !retrySleep(ctx, attempt) {
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("ark: 构造请求失败: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		if p.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		for k, v := range p.extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := p.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("ark: 请求发送失败: %w", err)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = fmt.Errorf("ark: 服务端错误 (状态码 %d): %s", resp.StatusCode, truncateBody(body))
			continue
		}

		return nil, fmt.Errorf("ark: 请求被拒绝 (状态码 %d): %s", resp.StatusCode, truncateBody(body))
	}

	return nil, fmt.Errorf("ark: 重试 %d 次后仍失败: %w", maxRetries, lastErr)
}

// streamResponse 解析火山引擎 SSE 流
//
// 格式与 OpenAI 兼容，但额外支持 reasoning_content 字段（豆包 thinking 模式）。
// 处理逻辑与 DeepSeek 的 reasoning_content 类似。
func (p *arkProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	defer close(out)
	defer resp.Body.Close()

	toolAccumulator := newToolCallAccumulator()
	reader := bufio.NewReader(resp.Body)
	inReasoning := false

	for {
		if ctx.Err() != nil {
			sendResponse(ctx, out, StreamResponse{Err: ctx.Err()})
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if inReasoning {
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{Content: "</think>\n\n"},
					})
				}
				return
			}
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("ark: 读取流失败: %w", err)})
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
		if data == "[DONE]" {
			if inReasoning {
				sendResponse(ctx, out, StreamResponse{
					Delta: Delta{Content: "</think>\n\n"},
				})
			}
			return
		}

		var chunk arkStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("ark: 解析数据块失败: %w", err)})
			continue
		}

		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]

			// 处理 reasoning_content（豆包 thinking 模式）
			if choice.Delta.ReasoningContent != "" {
				if !inReasoning {
					inReasoning = true
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{Content: "<think>\n"},
					})
				}
				sendResponse(ctx, out, StreamResponse{
					Delta: Delta{Content: choice.Delta.ReasoningContent},
				})
			}

			// 处理正常 content
			if choice.Delta.Content != "" {
				if inReasoning {
					inReasoning = false
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{Content: "</think>\n\n"},
					})
				}
				sendResponse(ctx, out, StreamResponse{
					Delta: Delta{Content: choice.Delta.Content},
				})
			}

			// 工具调用
			if len(choice.Delta.ToolCalls) > 0 {
				toolAccumulator.add(choice.Delta.ToolCalls)
			}

			// 结束原因
			if choice.FinishReason != "" {
				if inReasoning {
					inReasoning = false
					sendResponse(ctx, out, StreamResponse{
						Delta: Delta{Content: "</think>\n\n"},
					})
				}
				sr := StreamResponse{
					Delta: Delta{FinishReason: choice.FinishReason},
				}
				if choice.FinishReason == "tool_calls" {
					sr.Delta.ToolCalls = toolAccumulator.result()
				}
				sendResponse(ctx, out, sr)
			}
		}

		// Token 用量
		if chunk.Usage != nil {
			sendResponse(ctx, out, StreamResponse{
				Usage: &TokenUsage{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
					TotalTokens:      chunk.Usage.TotalTokens,
				},
			})
		}
	}
}
