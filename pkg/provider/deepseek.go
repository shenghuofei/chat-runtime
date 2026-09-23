// Package provider — DeepSeek 专用适配器。
//
// DeepSeek 的 API 基于 OpenAI 兼容格式，但有以下差异需要专门处理：
//   - 支持 reasoning_content 字段（思维链 / thinking 输出）
//   - 部分模型（deepseek-reasoner）不支持 temperature/top_p 参数
//   - 支持 FIM（Fill-in-the-Middle）补全模式
//   - 有独立的 prefix caching 统计（cache_creation_input_tokens / cache_read_input_tokens）
//   - 工具调用时支持 json_schema 参数约束
//
// 本适配器继承 openAIProvider 的核心实现，仅覆写差异部分。
//
// 参考文档: https://api-docs.deepseek.com/
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
	defaultDeepSeekBaseURL = "https://api.deepseek.com"
	defaultDeepSeekModel   = "deepseek-chat"
)

// deepSeekProvider 实现 DeepSeek API 的 Provider。
// 与 OpenAI 大部分兼容，但增加了 reasoning_content 的解析。
type deepSeekProvider struct {
	name         string
	model        string
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	httpClient   *http.Client
}

// NewDeepSeekProvider 创建 DeepSeek 专用 Provider。
func NewDeepSeekProvider(cfg ProviderConfig) (Provider, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultDeepSeekBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultDeepSeekModel
	}
	return &deepSeekProvider{
		name:         "deepseek",
		model:        model,
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		extraHeaders: cfg.ExtraHeaders,
		httpClient:   &http.Client{Timeout: requestTimeout},
	}, nil
}

func (p *deepSeekProvider) Name() string { return p.name }

func (p *deepSeekProvider) Close() error {
	p.httpClient.CloseIdleConnections()
	return nil
}

// ---- DeepSeek 特有的响应结构 ----

// deepSeekStreamChunk 扩展 OpenAI 格式，增加 reasoning_content 字段
type deepSeekStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"` // DeepSeek 思维链输出
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *deepSeekUsage `json:"usage"`
}

// deepSeekUsage DeepSeek 扩展的 token 用量，包含缓存统计
type deepSeekUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// DeepSeek 独有: prompt cache 统计
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
}

// Chat 实现 Provider 接口。
//
// DeepSeek 的请求格式与 OpenAI 完全兼容，但响应解析需要额外处理
// reasoning_content 字段（在思维链模式下，模型会先输出推理过程，
// 再输出最终回复）。
func (p *deepSeekProvider) Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error) {
	o := applyOptions(opts...)

	// DeepSeek 请求格式与 OpenAI 兼容，复用 convertMessages / convertTools
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

	// deepseek-reasoner 模型不支持 temperature/top_p
	if isReasonerModel(p.model) {
		reqBody.Temperature = nil
		reqBody.TopP = nil
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("deepseek: 序列化请求体失败: %w", err)
	}

	resp, err := p.doWithRetry(ctx, payload)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamResponse)
	go p.streamResponse(ctx, resp, out)
	return out, nil
}

// isReasonerModel 判断是否为推理模型（不支持 temperature/top_p）
func isReasonerModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "reasoner")
}

// doWithRetry 带重试的 HTTP 请求
func (p *deepSeekProvider) doWithRetry(ctx context.Context, payload []byte) (*http.Response, error) {
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
			return nil, fmt.Errorf("deepseek: 构造请求失败: %w", err)
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
			lastErr = fmt.Errorf("deepseek: 请求发送失败: %w", err)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = fmt.Errorf("deepseek: 服务端错误 (状态码 %d): %s", resp.StatusCode, truncateBody(body))
			continue
		}

		return nil, fmt.Errorf("deepseek: 请求被拒绝 (状态码 %d): %s", resp.StatusCode, truncateBody(body))
	}

	return nil, fmt.Errorf("deepseek: 重试 %d 次后仍失败: %w", maxRetries, lastErr)
}

// streamResponse 解析 DeepSeek SSE 流
//
// 与 OpenAI SSE 格式基本一致，但 delta 中可能包含额外的 reasoning_content 字段。
// reasoning_content 是模型的思维链输出（thinking 过程），会在最终 content 之前输出。
// 我们将 reasoning_content 用 <think> 标签包裹后合并到 content 中输出，
// 这样下游可以选择展示或隐藏思维过程。
func (p *deepSeekProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	defer close(out)
	defer resp.Body.Close()

	toolAccumulator := newToolCallAccumulator()
	reader := bufio.NewReader(resp.Body)

	// 追踪是否在 reasoning 阶段
	inReasoning := false

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
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("deepseek: 读取流失败: %w", err)})
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
			// 如果仍在 reasoning 中，补上关闭标签
			if inReasoning {
				sendResponse(ctx, out, StreamResponse{
					Delta: Delta{Content: "</think>\n\n"},
				})
			}
			return
		}

		var chunk deepSeekStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			sendResponse(ctx, out, StreamResponse{Err: fmt.Errorf("deepseek: 解析数据块失败: %w", err)})
			continue
		}

		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]

			// 处理 reasoning_content（思维链）
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
				// 从 reasoning 切换到 content
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

			// 工具调用累积
			if len(choice.Delta.ToolCalls) > 0 {
				toolAccumulator.add(choice.Delta.ToolCalls)
			}

			// 结束原因
			if choice.FinishReason != "" {
				// 补上 reasoning 关闭标签
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
