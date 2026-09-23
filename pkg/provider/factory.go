package provider

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Constructor 是 Provider 的构造函数类型，根据配置创建具体实例。
type Constructor func(cfg ProviderConfig) (Provider, error)

// registry 保存 类型名 -> 构造函数 的全局注册表。
var registry = struct {
	mu           sync.RWMutex
	constructors map[string]Constructor
}{
	constructors: make(map[string]Constructor),
}

// Register 注册一个 Provider 构造函数。
//
// name 为供应商类型标识（不区分大小写，内部统一转为小写存储）。
// 重复注册同名类型会覆盖之前的构造函数，便于自定义实现替换内置实现。
func Register(name string, ctor Constructor) {
	if name == "" || ctor == nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.constructors[strings.ToLower(name)] = ctor
}

// availableTypes 返回当前已注册的所有类型（已排序），用于错误提示。
func availableTypes() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	types := make([]string, 0, len(registry.constructors))
	for t := range registry.constructors {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// New 是工厂的核心方法：根据配置的 Type 查找构造函数并创建 Provider。
func New(cfg ProviderConfig) (Provider, error) {
	if cfg.Type == "" {
		return nil, fmt.Errorf("未指定 provider 类型 (config.Type 为空)")
	}

	registry.mu.RLock()
	ctor, ok := registry.constructors[strings.ToLower(cfg.Type)]
	registry.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("未知的 provider 类型 %q，当前支持: %v", cfg.Type, availableTypes())
	}

	return ctor(cfg)
}

// OpenAI 兼容 Provider 使用的默认 BaseURL。
//
// Ollama、Qwen（阿里通义）、OpenRouter 提供 OpenAI 兼容接口，
// 因此统一复用 openAIProvider，仅默认地址不同。
//
// Claude、Gemini、DeepSeek、Ark（火山方舟）各有专用适配器，
// 处理各自独特的 API 差异（鉴权方式、消息格式、thinking/reasoning 等）。
const (
	ollamaBaseURL     = "http://localhost:11434/v1"
	qwenBaseURL       = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	openRouterBaseURL = "https://openrouter.ai/api/v1"
)

// withDefaultBaseURL 返回一个构造函数：当配置未指定 BaseURL 时填充默认地址，
// 然后交由 OpenAI 兼容实现创建 Provider。
func withDefaultBaseURL(defaultURL string) Constructor {
	return func(cfg ProviderConfig) (Provider, error) {
		if cfg.BaseURL == "" {
			cfg.BaseURL = defaultURL
		}
		return NewOpenAIProvider(cfg)
	}
}

// init 注册所有内置供应商。
//
// 分为两类：
//   - 专用适配器：claude / gemini / deepseek / ark（处理各自独特的 API 差异）
//   - OpenAI 兼容：openai / ollama / qwen / openrouter（复用 openAIProvider）
func init() {
	// ---- 专用适配器 ----

	// Claude (Anthropic): x-api-key 鉴权、system 顶层字段、content blocks、
	// 独特的 SSE 事件格式 (message_start/content_block_delta/...)
	Register("claude", NewClaudeProvider)

	// Gemini (Google): URL query key 鉴权、systemInstruction 顶层字段、
	// functionCall/functionResponse 工具格式、user/model 角色
	Register("gemini", NewGeminiProvider)

	// DeepSeek: reasoning_content（思维链）、reasoner 模型参数限制、
	// prompt cache 统计
	Register("deepseek", NewDeepSeekProvider)

	// Ark (火山引擎方舟 / 豆包): endpoint ID 模型名、reasoning_content（thinking 模式）、
	// prompt_tokens_details 缓存统计
	Register("ark", NewArkProvider)

	// ---- OpenAI 兼容 ----

	// OpenAI：使用其默认地址（在 NewOpenAIProvider 内部处理）。
	Register("openai", NewOpenAIProvider)

	// Ollama: 本地部署，OpenAI 兼容接口。
	Register("ollama", withDefaultBaseURL(ollamaBaseURL))

	// Qwen (阿里通义千问): OpenAI 兼容接口。
	Register("qwen", withDefaultBaseURL(qwenBaseURL))

	// OpenRouter: 多模型聚合网关，OpenAI 兼容接口。
	Register("openrouter", withDefaultBaseURL(openRouterBaseURL))
}
