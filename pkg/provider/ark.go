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
// 本适配器通过嵌入 baseProvider 复用 HTTP 请求与重试逻辑，
// 通过 reasoningTracker 复用思维链状态管理。
//
// 参考文档: https://www.volcengine.com/docs/82379/1298454
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	defaultArkBaseURL = "https://ark.cn-beijing.volces.com/api/v3"
)

// arkProvider 实现火山引擎方舟 API 的 Provider。
// 嵌入 baseProvider 复用 HTTP 与重试逻辑。
type arkProvider struct {
	baseProvider
	// isReasoner 是否为推理模型（豆包 thinking 模式，不支持 temperature/top_p）。
	isReasoner bool
}

// NewArkProvider 创建火山引擎方舟专用 Provider。
func NewArkProvider(cfg ProviderConfig) (Provider, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultArkBaseURL
	}
	model := cfg.Model
	if model == "" {
		return nil, fmt.Errorf("ark: model（endpoint ID）不能为空，请在配置中指定，如 ep-xxxx")
	}
	return &arkProvider{
		baseProvider: baseProvider{
			name:         "ark",
			model:        model,
			baseURL:      baseURL,
			apiKey:       cfg.APIKey,
			extraHeaders: cfg.ExtraHeaders,
			httpClient:   newHTTPClient(),
		},
		isReasoner: cfg.IsReasoner,
	}, nil
}

// ---- Ark 特有的响应结构 ----
//
// 注：流式响应解析已统一由 base.go 的 streamOpenAICompat 处理，
// Ark 的 reasoning_content 与基础 usage 字段均包含在通用结构 openAICompatChunk 中。
// Ark 独有的 prompt_tokens_details（cached_tokens）当前未被使用，故不再单独定义结构体。

// Chat 实现 Provider 接口。
func (p *arkProvider) Chat(ctx context.Context, messages []Message, tools []ToolDef, opts ...Option) (<-chan StreamResponse, error) {
	o := applyOptions(opts...)

	// Ark 请求格式与 OpenAI 兼容，复用公共构建器。
	reqBody := buildOpenAICompatRequest(p.model, messages, tools, o)
	// 豆包 thinking（推理）模型不支持 temperature/top_p。
	if p.isReasoner {
		reqBody.Temperature = nil
		reqBody.TopP = nil
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("ark: 序列化请求体失败: %w", err)
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

// streamResponse 解析火山引擎 SSE 流。
//
// Ark 使用标准 OpenAI 兼容格式并支持 reasoning_content（豆包 thinking），
// 因此直接复用 base.go 的通用解析函数 streamOpenAICompat。
func (p *arkProvider) streamResponse(ctx context.Context, resp *http.Response, out chan<- StreamResponse) {
	streamOpenAICompat(ctx, resp, out, openAICompatStreamConfig{providerName: "ark", hasReasoning: true})
}
