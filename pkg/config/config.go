// Package config 负责解析 chat-runtime 的 YAML 配置文件。
//
// 配置由 providers / models / chats / mcp_servers / tools / context_manager / server
// 七个顶层字段组成，各段之间通过"名称引用"串联。详见 docs/configuration.md。
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
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
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
	// MCPInitTimeout 单个 MCP Server 初始化（连接 + 握手 + 工具发现）的超时上限。
	// 默认 30s；网络环境较差或 stdio 进程启动慢时可适当调大。
	MCPInitTimeout time.Duration `yaml:"mcp_init_timeout"`
}

// ProviderConfig 描述一个 LLM 提供商的连接信息。
type ProviderConfig struct {
	// Type Provider 类型：openai / claude / deepseek / ollama / ark / gemini / qwen。
	Type string `yaml:"type"`
	// APIKey API 密钥，建议通过环境变量注入。
	APIKey string `yaml:"api_key"`
	// BaseURL 自定义 API endpoint（代理 / 私有部署 / 兼容网关）。
	BaseURL string `yaml:"base_url"`
	// ExtraHeaders 额外的自定义请求头（如代理鉴权、路由标记等）。
	ExtraHeaders map[string]string `yaml:"headers"`
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
	// Reasoner 显式声明该模型为推理模型（如 deepseek-reasoner、o1 等）。
	// 推理模型通常不支持 temperature/top_p，且可能使用不同的 max_tokens 字段名。
	// 若不设置，某些 Provider 会根据模型名做启发式判断。
	Reasoner bool `yaml:"reasoner"`
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
	// InitTimeout 该 Server 初始化（连接 + 握手 + 工具发现）的超时，覆盖全局 MCPInitTimeout。
	// 为 0 时使用全局值。stdio 模式子进程启动较慢，建议设大（如 60s）；
	// SSE/HTTP 模式通常 10s 以内即可。
	InitTimeout time.Duration `yaml:"init_timeout"`
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
	// MaxOutputBytes 单次命令输出的最大字节数，超出后采用"头尾各保留 50%"截取策略。
	// 0 或未设置时使用默认值（1 MiB）。可设为 -1 表示不限制（谨慎使用）。
	MaxOutputBytes int `yaml:"max_output_bytes"`
	// BlockedPatterns 额外的危险命令正则模式（追加到内置规则之后）。
	// 格式为 Go 正则表达式字符串列表，编译失败时跳过并记录警告。
	BlockedPatterns []string `yaml:"blocked_patterns"`
}

// FilesystemToolConfig 文件操作工具配置（预留，当前未实现对应工具）。
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

	// MaxSessions Web 模式最大并发会话数，防止连接数无上限导致 DoS。
	// 0 或不设置：使用默认值 100；
	// 负数（如 -1）：不限制会话数量（谨慎在公网环境使用）。
	MaxSessions int `yaml:"max_sessions"`

	// AllowedOrigins CORS 跨域白名单，支持精确 Origin 字符串或 "*"（放行所有来源）。
	// 为空时不启用 CORS 中间件，浏览器跨域请求将被拒绝。
	// 示例：["https://app.example.com", "http://localhost:3000"]
	AllowedOrigins []string `yaml:"allowed_origins"`

	// WebSocket 与审批超时配置（均有默认值，通常无需手动设置）。

	// ApprovalTimeout Web 审批等待超时，默认 5m。
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`
	// ShutdownTimeout 优雅关闭的最大等待时间，默认 15s。
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// ReadHeaderTimeout HTTP 读取请求头的超时，默认 10s。
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// PingInterval WebSocket 心跳发送间隔，默认 30s。
	PingInterval time.Duration `yaml:"ping_interval"`
	// PongWait 等待 pong 响应的超时（应大于 PingInterval），默认 45s。
	PongWait time.Duration `yaml:"pong_wait"`
	// WriteWait WebSocket 写操作超时，默认 10s。
	WriteWait time.Duration `yaml:"write_wait"`
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
	expanded, undefined := expandEnv(raw)
	if len(undefined) > 0 {
		slog.Warn("配置中引用了未定义的环境变量，已替换为空字符串",
			"path", path,
			"variables", undefined,
		)
	}

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
//
// 第二个返回值是所有"在系统环境中未定义"的变量名列表（去重、按出现顺序），
// 供上层在必要时发出警告，避免因变量拼写错误或漏配而静默替换为空串。
//
// 安全注意：若替换后的值含有 YAML 特殊字符（如 " #"、": "、"{" 等），
// 会用 YAML 单引号字符串包裹，防止破坏解析结构。对嵌入式用法
// （如 "Bearer ${TOKEN}"）通常无影响，因为 API Key / JWT 仅含 base64url 字符集。
func expandEnv(data []byte) ([]byte, []string) {
	var undefined []string
	seen := make(map[string]bool)
	result := envVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		// match 形如 ${VAR}，截取中间的变量名。
		name := string(match[2 : len(match)-1])
		val, ok := os.LookupEnv(name)
		if !ok && !seen[name] {
			seen[name] = true
			undefined = append(undefined, name)
		}
		return yamlSafeEnvValue(val)
	})
	return result, undefined
}

// yamlSafeEnvValue 将环境变量的值转换为 YAML 安全的字节序列。
//
// 若值含有会被 YAML 解析器特殊处理的字符，则用单引号字符串包裹
// （内部的单引号以两个连续单引号转义）。普通 API Key / URL / Path 不含这些字符，
// 直接返回原始字节，不影响 "Bearer ${TOKEN}" 等复合字符串的拼接语义。
func yamlSafeEnvValue(val string) []byte {
	if needsYAMLQuoting(val) {
		escaped := strings.ReplaceAll(val, "'", "''")
		return []byte("'" + escaped + "'")
	}
	return []byte(val)
}

// needsYAMLQuoting 判断值是否需要 YAML 引号保护。
func needsYAMLQuoting(val string) bool {
	if val == "" {
		return false
	}
	// " #"（空格+井号）：YAML 解析器视后续内容为注释，导致值被静默截断。
	if strings.Contains(val, " #") {
		return true
	}
	// ": "（冒号+空格）：YAML 映射分隔符，可能引起结构混乱或解析错误。
	if strings.Contains(val, ": ") {
		return true
	}
	// 控制字符（\t \n \r）：YAML 对嵌入的制表符/换行符有特殊折叠语义，
	// 用引号包裹后由 YAML 解析器还原原始字节序列，避免语义扭曲。
	if strings.ContainsAny(val, "\t\n\r") {
		return true
	}
	// 空字节（\x00）：YAML 规范不允许 NULL 字符出现在普通标量中。
	if strings.ContainsRune(val, 0) {
		return true
	}
	// YAML 保留的布尔/空值字面量：不引用时会被解析器解释为 bool 或 null，
	// 导致本应是字符串的 API Key / 配置项被静默转型。
	// 涵盖 YAML 1.1（yes/no/on/off）与 YAML 1.2（true/false/null/~）两套规范。
	switch strings.ToLower(val) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return true
	}
	// 值以 YAML 流指示符或特殊首字符起始时，plain scalar 解析会出错。
	// 额外补充：'-'（可解析为列表项）、'?'（显式映射键）、':'（映射分隔符）、
	// '#'（注释起始，某些解析器允许首字符但为安全起见引用）、','（流序列分隔符）。
	switch val[0] {
	case '{', '[', '!', '&', '*', '|', '>', '\'', '"', '%', '@', '`',
		'-', '?', ':', '#', ',':
		return true
	}
	return false
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
	cwd, err := os.Getwd()
	if err != nil {
		// 工作目录不可访问时（如被删除或权限变更），给出告警而非静默使用空字符串，
		// 避免模板变量 {{.Cwd}} 渲染为空白时难以排查。
		slog.Warn("获取当前工作目录失败，{{.Cwd}} 模板变量将为空", "error", err)
	}
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
		// 安全限制：@file: 只允许相对路径，避免配置文件来自不可信来源时读取任意系统文件。
		if filepath.IsAbs(rel) {
			return "", fmt.Errorf("@file: 路径必须为相对路径，不允许绝对路径：%s", rel)
		}
		p := filepath.Join(baseDir, rel)
		// 路径穿越检查（一）：lexical 层面——基于 filepath.Clean 快速排除显式越界路径。
		// 不允许访问 baseDir 本身（必须是目录内的文件），也不允许 ".." 逃逸。
		cleanBase := filepath.Clean(baseDir)
		cleanP := filepath.Clean(p)
		if !strings.HasPrefix(cleanP, cleanBase+string(filepath.Separator)) {
			return "", fmt.Errorf("@file: 路径越界，不允许访问配置目录之外的文件：%s", rel)
		}
		// 路径穿越检查（二）：使用 os.Lstat 拒绝符号链接，彻底消除"检查→读取"之间的 TOCTOU 窗口。
		// 旧的 EvalSymlinks 方案存在竞争：攻击者可在检查通过后、ReadFile 之前把符号链接
		// 替换为指向 baseDir 外的路径；直接拒绝符号链接可从根本上消除该攻击面。
		if lfi, lerr := os.Lstat(p); lerr == nil && lfi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("@file: 不允许使用符号链接，请直接引用目标文件：%s", rel)
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
	if c.Server.MaxSessions == 0 {
		c.Server.MaxSessions = 100
	}
	if c.Server.ApprovalTimeout == 0 {
		c.Server.ApprovalTimeout = 5 * time.Minute
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 15 * time.Second
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = 10 * time.Second
	}
	if c.Server.PingInterval == 0 {
		c.Server.PingInterval = 30 * time.Second
	}
	if c.Server.PongWait == 0 {
		c.Server.PongWait = 45 * time.Second
	}
	if c.Server.WriteWait == 0 {
		c.Server.WriteWait = 10 * time.Second
	}
	if c.MCPInitTimeout == 0 {
		c.MCPInitTimeout = 30 * time.Second
	}
}

// Validate 对配置进行基本的引用一致性校验。
//
// 注意：为兼容仅测试局部字段的单元测试（如只提供 mcp_servers 或 chats 的片段），
// 校验仅在相关引用存在时才检查其一致性，不强制要求所有顶层字段都非空。
func (c *Config) Validate() error {
	// BasicAuth：启用时 username / password 不得为空。
	if c.Server.BasicAuth.Enabled {
		if c.Server.BasicAuth.Username == "" || c.Server.BasicAuth.Password == "" {
			return fmt.Errorf("配置校验失败: basic_auth.enabled=true 但 username 或 password 为空")
		}
	}

	// 检查是否有多个 default: true 的 chat；多个时 map 遍历随机，行为不确定，直接报错。
	defaultCount := 0
	for _, ch := range c.Chats {
		if ch.Default {
			defaultCount++
		}
	}
	if defaultCount > 1 {
		return fmt.Errorf("配置校验失败: 有 %d 个 chat 设置了 default: true，最多只能有一个", defaultCount)
	}

	// 校验 provider 的 BaseURL 格式：非空时必须以 http:// 或 https:// 开头。
	for name, p := range c.Providers {
		if p.BaseURL != "" &&
			!strings.HasPrefix(p.BaseURL, "http://") &&
			!strings.HasPrefix(p.BaseURL, "https://") {
			return fmt.Errorf("配置校验失败: provider %q base_url 必须以 http:// 或 https:// 开头，当前值: %q", name, p.BaseURL)
		}
	}

	// 校验 model 引用的 provider 存在，并检查采样参数范围。
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
		// temperature 0 表示"使用 Provider 默认值"，不校验；非 0 时须在 [0, 2]。
		if m.Temperature != 0 && (m.Temperature < 0 || m.Temperature > 2) {
			return fmt.Errorf("配置校验失败: model %q temperature=%.2f 超出有效范围 [0, 2]", name, m.Temperature)
		}
		// top_p 0 表示"使用 Provider 默认值"，不校验；非 0 时须在 (0, 1]。
		if m.TopP != 0 && (m.TopP < 0 || m.TopP > 1) {
			return fmt.Errorf("配置校验失败: model %q top_p=%.2f 超出有效范围 (0, 1]", name, m.TopP)
		}
		// max_tokens 0 表示"使用 Provider 默认值"，不校验；负值无意义。
		if m.MaxTokens < 0 {
			return fmt.Errorf("配置校验失败: model %q max_tokens=%d 不能为负数", name, m.MaxTokens)
		}
	}

	// 校验不超过一个 chat 设置了 default: true，避免 map 遍历随机性导致默认对话不确定。
	defaultChatCount := 0
	var firstDefaultChat string
	for name, ch := range c.Chats {
		if ch.Default {
			defaultChatCount++
			if firstDefaultChat == "" {
				firstDefaultChat = name
			}
		}
	}
	if defaultChatCount > 1 {
		return fmt.Errorf("配置校验失败: 有 %d 个 chat 设置了 default: true（含 %q），最多只能有一个", defaultChatCount, firstDefaultChat)
	}

	// 校验 chat 引用的 model 与 mcp_servers 存在（仅当 models 已定义时校验 model 引用）。
	for name, ch := range c.Chats {
		// 若已配置 models，则每个 chat 必须声明 model；否则无法确定使用哪个 LLM。
		if len(c.Models) > 0 && ch.Model == "" {
			return fmt.Errorf("配置校验失败: chat %q 未指定 model", name)
		}
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

// Chat 返回指定名称的对话配置；name 为空时按以下优先级解析默认对话：
//  1. 显式标记 default: true 的对话；
//  2. 名为 "default" 的对话；
//  3. 配置中恰好只有一个对话时，直接返回该对话（方便最简配置）。
func (c *Config) Chat(name string) (ChatConfig, error) {
	if name == "" {
		// 1. 优先返回显式标记 default: true 的对话。
		for _, ch := range c.Chats {
			if ch.Default {
				return ch, nil
			}
		}
		// 2. 再尝试名为 "default" 的对话。
		if ch, ok := c.Chats["default"]; ok {
			return ch, nil
		}
		// 3. 最后回退：仅有一个对话时直接返回，方便最简配置无需显式命名。
		if len(c.Chats) == 1 {
			for _, ch := range c.Chats {
				return ch, nil
			}
		}
		return ChatConfig{}, fmt.Errorf(
			"未找到默认 chat 配置（共有 %d 个 chat，均未标记 default: true，也无名为 default 的 chat）",
			len(c.Chats),
		)
	}
	ch, ok := c.Chats[name]
	if !ok {
		return ChatConfig{}, fmt.Errorf("未找到名为 %q 的 chat 配置", name)
	}
	return ch, nil
}

// ChatNames 返回所有已定义的对话名称（按字母顺序排列，保证响应顺序稳定）。
func (c *Config) ChatNames() []string {
	names := make([]string, 0, len(c.Chats))
	for name := range c.Chats {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
