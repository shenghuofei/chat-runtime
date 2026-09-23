// Package config 负责解析 chat-runtime 的 YAML 配置文件。
//
// 配置由 providers / models / chats / mcp_servers / tools / context_manager / server
// 七个顶层字段组成，各段之间通过“名称引用”串联。详见 docs/configuration.md。
//
// 加载时的处理链：
//  1. ${VAR} 环境变量插值（未定义变量替换为空字符串）；
//  2. YAML 反序列化；
//  3. 名称回填（如 MCP Server 的 Name）；
//  4. system 提示词的 @file: 引用解析与 Go 模板变量渲染；
//  5. 默认值填充与基本的引用一致性校验。
package config

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/manager"
	"gopkg.in/yaml.v3"
)

// envVarPattern 匹配 ${VAR} 形式的环境变量占位符。
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// filePrefix 是 system 提示词从文件加载的前缀标记。
const filePrefix = "@file:"

// Config 是顶层配置结构，对应整份 YAML 文件。
type Config struct {
	// Providers LLM 提供商定义，key 为自定义名称，供 Models 引用。
	Providers map[string]ProviderConfig `yaml:"providers"`
	// Models 模型定义，key 为自定义名称，供 Chats 引用。
	Models map[string]ModelConfig `yaml:"models"`
	// Chats 对话定义，key 为自定义名称。默认加载名为 default 的对话。
	Chats map[string]ChatConfig `yaml:"chats"`
	// MCPServers MCP 工具来源定义。
	MCPServers map[string]MCPServerConfig `yaml:"mcp_servers"`
	// Tools 内置工具配置。
	Tools ToolsConfig `yaml:"tools"`
	// ContextManager 全局上下文溢出策略（可被 Chat 级别覆盖）。
	// 直接复用 manager 包的配置结构，避免重复定义与转换。
	ContextManager manager.ManagerConfig `yaml:"context_manager"`
	// Server Web 服务模式配置。
	Server ServerConfig `yaml:"server"`
}

// ProviderConfig 描述一个 LLM 提供商的连接信息。
type ProviderConfig struct {
	// Type Provider 类型：openai / claude / deepseek / ollama / ark / gemini / qwen。
	Type string `yaml:"type"`
	// APIKey API 密钥，建议通过环境变量注入。
	APIKey string `yaml:"api_key"`
	// BaseURL 自定义 API endpoint（代理 / 私有部署 / 兼容网关）。
	BaseURL string `yaml:"base_url"`
	// Timeout 请求超时时间，如 60s、2m。
	Timeout time.Duration `yaml:"timeout"`
}

// ModelConfig 描述一个模型，绑定 provider 并指定模型名与采样参数。
type ModelConfig struct {
	// Provider 引用 Providers 中的名称。
	Provider string `yaml:"provider"`
	// Model Provider 侧的实际模型标识，如 gpt-4o、deepseek-chat。
	Model string `yaml:"model"`
	// Temperature 采样温度，0~2。
	Temperature float64 `yaml:"temperature"`
	// MaxTokens 单次响应的最大生成 token 数。
	MaxTokens int `yaml:"max_tokens"`
	// TopP 核采样参数。
	TopP float64 `yaml:"top_p"`
}

// ChatConfig 描述一个对话，绑定 model 并定义提示词、工具与上下文策略。
type ChatConfig struct {
	// Model 引用 Models 中的名称。
	Model string `yaml:"model"`
	// System 系统提示词，支持 @file: 引用与 Go 模板变量。
	System string `yaml:"system"`
	// Default 是否为默认对话（未显式指定 --chat 时加载）。
	Default bool `yaml:"default"`
	// MaxIterations Tool Calling 循环的最大迭代次数（防止无限循环）。
	MaxIterations int `yaml:"max_iterations"`
	// Tools 该对话可用的工具，缺省表示全部可用。
	Tools []string `yaml:"tools"`
	// MCPServers 该对话启用的 MCP Server 名称，缺省表示全部启用。
	MCPServers []string `yaml:"mcp_servers"`
	// ContextManager 覆盖全局 ContextManager，可选。
	ContextManager *manager.ManagerConfig `yaml:"context_manager"`
}

// MCPServerConfig 描述一个 MCP Server（工具来源）。
type MCPServerConfig struct {
	// Name Server 名称，由加载时从 map 的 key 回填（不参与 YAML 解析）。
	Name string `yaml:"-"`
	// Transport 传输方式：stdio / sse / streamable-http。
	Transport string `yaml:"transport"`
	// Command stdio 模式下要启动的可执行文件。
	Command string `yaml:"command"`
	// Args stdio 模式下的命令行参数。
	Args []string `yaml:"args"`
	// Env stdio 模式下注入子进程的环境变量。
	Env map[string]string `yaml:"env"`
	// URL sse / streamable-http 模式的服务地址。
	URL string `yaml:"url"`
	// Headers sse / streamable-http 模式的自定义请求头。
	Headers map[string]string `yaml:"headers"`
	// AutoApprove 审批策略：true 全部自动 / [tool...] 指定 / false 全部需审批。
	AutoApprove AutoApprove `yaml:"auto_approve"`
	// Include 工具白名单，仅暴露列出的工具。
	Include []string `yaml:"include"`
	// Exclude 工具黑名单，屏蔽列出的工具。
	Exclude []string `yaml:"exclude"`
	// NoConcurrent 该 Server 的工具调用整体串行执行。
	NoConcurrent bool `yaml:"no_concurrent"`
	// NoConcurrentTools 指定这些工具之间串行执行。
	NoConcurrentTools []string `yaml:"no_concurrent_tools"`
}

// ToolsConfig 内置工具配置。
type ToolsConfig struct {
	// Command 命令执行工具配置。
	Command CommandToolConfig `yaml:"command"`
	// Filesystem 文件操作工具配置。
	Filesystem FilesystemToolConfig `yaml:"filesystem"`
}

// CommandToolConfig 命令执行工具配置。
type CommandToolConfig struct {
	// Enabled 是否启用命令执行工具。
	Enabled bool `yaml:"enabled"`
	// Whitelist 命令白名单，仅允许列出的命令执行。
	Whitelist []string `yaml:"whitelist"`
	// AutoApprove 命令执行的审批策略，规则同 MCP 的 auto_approve。
	AutoApprove AutoApprove `yaml:"auto_approve"`
	// Timeout 单条命令的执行超时（原始字符串，如 "30s"）。
	Timeout string `yaml:"timeout"`
}

// FilesystemToolConfig 文件操作工具配置。
type FilesystemToolConfig struct {
	// Enabled 是否启用文件操作工具。
	Enabled bool `yaml:"enabled"`
	// Root 文件操作限定的根目录，越界访问被拒绝。
	Root string `yaml:"root"`
	// Readonly 是否只读，禁止写入 / 删除。
	Readonly bool `yaml:"readonly"`
}

// ServerConfig Web 服务模式配置。
type ServerConfig struct {
	// Host 监听地址，默认 0.0.0.0。
	Host string `yaml:"host"`
	// Port 监听端口，默认 8080。
	Port int `yaml:"port"`
	// BasicAuth HTTP Basic Auth 配置。
	BasicAuth BasicAuthConfig `yaml:"basic_auth"`
}

// BasicAuthConfig HTTP Basic Auth 配置。
type BasicAuthConfig struct {
	// Enabled 是否启用 Basic Auth。
	Enabled bool `yaml:"enabled"`
	// Username 用户名。
	Username string `yaml:"username"`
	// Password 密码，建议通过环境变量注入。
	Password string `yaml:"password"`
}

// AutoApprove 表示审批策略，支持三种取值：
//   - true         全部工具自动批准
//   - [tool, ...]  仅列出的工具自动批准
//   - false / 省略  全部需人工审批
//
// 通过自定义 UnmarshalYAML 兼容 bool 与 list 两种 YAML 写法。
type AutoApprove struct {
	// All 为 true 时表示全部自动批准。
	All bool
	// Tools 指定自动批准的工具列表。
	Tools []string
}

// UnmarshalYAML 兼容解析 bool 或 string 列表两种形式。
func (a *AutoApprove) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		// 标量：解析为 bool（true / false）。
		var b bool
		if err := value.Decode(&b); err != nil {
			return fmt.Errorf("auto_approve 标量必须为布尔值: %w", err)
		}
		a.All = b
		a.Tools = nil
		return nil
	case yaml.SequenceNode:
		// 序列：解析为工具名列表。
		var tools []string
		if err := value.Decode(&tools); err != nil {
			return fmt.Errorf("auto_approve 列表必须为字符串数组: %w", err)
		}
		a.All = false
		a.Tools = tools
		return nil
	default:
		return fmt.Errorf("auto_approve 只支持布尔值或字符串列表")
	}
}

// Approved 判断给定工具名是否被配置为自动批准。
func (a AutoApprove) Approved(tool string) bool {
	if a.All {
		return true
	}
	for _, t := range a.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// Load 从指定路径读取并解析配置文件，完成环境变量插值、名称回填、
// system 提示词解析（@file: 与模板渲染）、默认值填充与基本校验。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	// 1. 环境变量插值：将 ${VAR} 替换为对应环境变量的值。
	expanded := expandEnv(raw)

	// 2. YAML 反序列化。
	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	// 3. 名称回填：将 map key 写入对应结构体的 Name 字段。
	for name, srv := range cfg.MCPServers {
		srv.Name = name
		cfg.MCPServers[name] = srv
	}

	// 4. 解析 system 提示词：@file: 引用（相对配置文件目录）+ Go 模板渲染。
	baseDir := filepath.Dir(path)
	tmplData := templateData()
	for name, ch := range cfg.Chats {
		resolved, err := resolveSystem(ch.System, baseDir, tmplData)
		if err != nil {
			return nil, fmt.Errorf("解析 chat %q 的 system 提示词失败: %w", name, err)
		}
		ch.System = resolved
		cfg.Chats[name] = ch
	}

	// 5. 默认值填充与校验。
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandEnv 将内容中的 ${VAR} 替换为环境变量的值（未定义则替换为空字符串）。
func expandEnv(data []byte) []byte {
	return envVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		// match 形如 ${VAR}，截取中间的变量名。
		name := string(match[2 : len(match)-1])
		return []byte(os.Getenv(name))
	})
}

// templateVars 是 system 提示词可用的 Go 模板变量集合。
type templateVars struct {
	// Cwd 当前工作目录。
	Cwd string
	// Home 当前用户主目录。
	Home string
	// User 当前用户名。
	User string
	// Date 当前日期（yyyy-MM-dd）。
	Date string
}

// templateData 收集运行时的模板变量。
func templateData() templateVars {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	return templateVars{
		Cwd:  cwd,
		Home: home,
		User: username,
		Date: time.Now().Format("2006-01-02"),
	}
}

// resolveSystem 解析 system 提示词：
//   - 以 @file: 开头时从文件读取内容（路径相对 baseDir）；
//   - 无论内联还是文件内容，都会经过 Go text/template 渲染。
func resolveSystem(system, baseDir string, data templateVars) (string, error) {
	if system == "" {
		return "", nil
	}

	content := system
	if strings.HasPrefix(system, filePrefix) {
		rel := strings.TrimSpace(strings.TrimPrefix(system, filePrefix))
		p := rel
		if !filepath.IsAbs(rel) {
			p = filepath.Join(baseDir, rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("读取 system 文件 %s 失败: %w", p, err)
		}
		content = string(b)
	}

	// Go 模板渲染。
	tmpl, err := template.New("system").Parse(content)
	if err != nil {
		return "", fmt.Errorf("解析 system 模板失败: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("渲染 system 模板失败: %w", err)
	}
	return buf.String(), nil
}

// applyDefaults 为可选字段填充默认值。
func (c *Config) applyDefaults() {
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
}

// Validate 对配置进行基本的引用一致性校验。
//
// 注意：为兼容仅测试局部字段的单元测试（如只提供 mcp_servers 或 chats 的片段），
// 校验仅在相关引用存在时才检查其一致性，不强制要求所有顶层字段都非空。
func (c *Config) Validate() error {
	// 校验 model 引用的 provider 存在。
	for name, m := range c.Models {
		if m.Provider == "" {
			return fmt.Errorf("配置校验失败: model %q 未指定 provider", name)
		}
		if _, ok := c.Providers[m.Provider]; !ok {
			return fmt.Errorf("配置校验失败: model %q 引用了不存在的 provider %q", name, m.Provider)
		}
		if m.Model == "" {
			return fmt.Errorf("配置校验失败: model %q 未指定 model 名称", name)
		}
	}

	// 校验 chat 引用的 model 与 mcp_servers 存在（仅当 models 已定义时校验 model 引用）。
	for name, ch := range c.Chats {
		if ch.Model != "" && len(c.Models) > 0 {
			if _, ok := c.Models[ch.Model]; !ok {
				return fmt.Errorf("配置校验失败: chat %q 引用了不存在的 model %q", name, ch.Model)
			}
		}
		for _, s := range ch.MCPServers {
			if _, ok := c.MCPServers[s]; !ok {
				return fmt.Errorf("配置校验失败: chat %q 引用了不存在的 mcp_server %q", name, s)
			}
		}
	}
	return nil
}

// Chat 返回指定名称的对话配置；name 为空时优先返回标记为 Default 的对话，
// 否则回退到名为 default 的对话。
func (c *Config) Chat(name string) (ChatConfig, error) {
	if name == "" {
		// 优先返回显式标记 default: true 的对话。
		for _, ch := range c.Chats {
			if ch.Default {
				return ch, nil
			}
		}
		name = "default"
	}
	ch, ok := c.Chats[name]
	if !ok {
		return ChatConfig{}, fmt.Errorf("未找到名为 %q 的 chat 配置", name)
	}
	return ch, nil
}

// ChatNames 返回所有已定义的对话名称。
func (c *Config) ChatNames() []string {
	names := make([]string, 0, len(c.Chats))
	for name := range c.Chats {
		names = append(names, name)
	}
	return names
}
