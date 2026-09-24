// Package provider — DeepSeek 专用适配器。
//
// DeepSeek 的 API 基于 OpenAI 兼容格式，但有以下差异需要专门处理：
//   - 支持 reasoning_content 字段（思维链 / thinking 输出）
//   - 部分模型（deepseek-reasoner）不支持 temperature/top_p 参数
//   - 支持 FIM（Fill-in-the-Middle）补全模式
//   - 有独立的 prefix caching 统计（cache_creation_input_tokens / cache_read_input_tokens）
//   - 工具调用时支持 json_schema 参数约束
//
// 本适配器通过嵌入 baseProvider 复用 HTTP 请求与重试逻辑，
// 通过 reasoningTracker 复用思维链状态管理。
//
// 参考文档: https://api-docs.deepseek.com/
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	defaultDeepSeekBaseURL = "https://api.deepseek.com"
	defaultDeepSeekModel   = "deepseek-chat"
)

// deepSeekProvider 实现 DeepSeek API 的 Provider。
// 嵌入 baseProvider 复用 HTTP 与重试逻辑。
type deepSeekProvider struct {
	baseProvider
	// isReasoner 是否为推理模型（不支持 temperature/top_p）。
	// 由配置显式声明优先；未声明时根据模型名启发式判断。
	isReasoner bool
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
		baseProvider: baseProvider{
			name:         "deepseek",
			model:        model,
			baseURL:      baseURL,
			apiKey:       cfg.APIKey,
			extraHeaders: cfg.ExtraHeaders,
			httpClient:   newHTTPClient(),
		},
		// 配置显式声明优先；未声明时根据模型名启发式判断（含 "reasoner" 子串）。
		isReasoner: cfg.IsReasoner || isReasonerModel(model),
	}, nil
}

// ---- DeepSeek 特有的响应结构 ----
//
// 注：流式响应解析已统一由 base.go 的 streamOpenAICompat 处理，
// DeepSeek 的 reasoning_content 与基础 usage 字段均包含在通用结构 openAICompatChunk 中。
// DeepSeek 独有的 prompt cache 统计（prompt_cache_hit_tokens 等）当前未被使用，故不再单独定义结构体。

// Chat 实现 Provider 接口。
//
// DeepSeek 的请求格式与 OpenAI 完全兼容，但响应解析需要额外处理
// reasoning_content 字段（在思维链模式下，模型会先输出推理过程，
// 再输出最终回复）。
func (p *deepSeekProvider) Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error) {
	o := applyOptions(opts...)

	// DeepSeek 请求格式与 OpenAI 兼容，复用公共构建器。
	reqBody := buildOpenAICompatRequest(p.model, messages, tools, o)

	// deepseek-reasoner 模型不支持 temperature/top_p。
	if p.isReasoner {
		reqBody.Temperature = nil
		reqBody.TopP = nil
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("deepseek: 序列化请求体失败: %w", err)
	}

	url := p.baseURL + "/chat/completions"
	resp, err := p.doWithRetry(ctx, url, payload, headerBearer, nil)
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

// streamResponse 解析 DeepSeek SSE 流。
//
// DeepSeek 使用标准 OpenAI 兼容格式并支持 reasoning_content（思维链），
// 因此直接复用 base.go 的通用解析函数 streamOpenAICompat。
func (p *deepSeekProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	streamOpenAICompat(ctx, resp, out, openAICompatStreamConfig{providerName: "deepseek", hasReasoning: true})
}
