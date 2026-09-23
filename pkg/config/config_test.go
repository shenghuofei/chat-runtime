package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/manager"
)

// writeFile 在临时目录写入文件并返回其路径。
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写入测试文件失败：%v", err)
	}
	return p
}

// TestLoadConfig 验证基本配置文件的加载与各字段解析。
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	yml := `
providers:
  deepseek:
    type: deepseek
    api_key: test-key
    base_url: https://api.deepseek.com
    timeout: 60s

models:
  default:
    provider: deepseek
    model: deepseek-chat
    temperature: 0.7
    max_tokens: 4096
    top_p: 1.0

chats:
  default:
    model: default
    system: "你是一个有用的助手。"
    default: true
    max_iterations: 10
    tools:
      - command
    mcp_servers:
      - k8s-eye

mcp_servers:
  k8s-eye:
    transport: stdio
    command: ./k8s-eye
    args: ["--verbose"]
    auto_approve:
      - get_pods
    exclude:
      - delete_namespace

tools:
  command:
    enabled: true
    whitelist: [ls, cat]
    auto_approve: [ls]
    timeout: 30s
  filesystem:
    enabled: true
    root: ./workspace

context_manager:
  mode: truncate
  max_rounds: 20

server:
  host: 0.0.0.0
  port: 8080
  basic_auth:
    enabled: true
    username: admin
    password: secret
`
	path := writeFile(t, dir, "config.yml", yml)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}

	// Providers
	if cfg.Providers["deepseek"].Type != "deepseek" {
		t.Errorf("provider type 解析错误：%+v", cfg.Providers["deepseek"])
	}
	// Models
	if cfg.Models["default"].Temperature != 0.7 || cfg.Models["default"].MaxTokens != 4096 {
		t.Errorf("model 参数解析错误：%+v", cfg.Models["default"])
	}
	// Chats
	chat := cfg.Chats["default"]
	if !chat.Default || chat.MaxIterations != 10 || chat.Model != "default" {
		t.Errorf("chat 解析错误：%+v", chat)
	}
	if len(chat.MCPServers) != 1 || chat.MCPServers[0] != "k8s-eye" {
		t.Errorf("chat mcp_servers 解析错误：%+v", chat.MCPServers)
	}
	// MCP Server：名称回填 + auto_approve 列表 + exclude
	srv := cfg.MCPServers["k8s-eye"]
	if srv.Name != "k8s-eye" {
		t.Errorf("MCP Server 名称未回填：%+v", srv)
	}
	if srv.AutoApprove.All || len(srv.AutoApprove.Tools) != 1 || srv.AutoApprove.Tools[0] != "get_pods" {
		t.Errorf("auto_approve 列表解析错误：%+v", srv.AutoApprove)
	}
	// Tools
	if !cfg.Tools.Command.Enabled || cfg.Tools.Command.Timeout != "30s" {
		t.Errorf("command 工具解析错误：%+v", cfg.Tools.Command)
	}
	// ContextManager
	if cfg.ContextManager.Mode != manager.ModeTruncate || cfg.ContextManager.MaxRounds != 20 {
		t.Errorf("context_manager 解析错误：%+v", cfg.ContextManager)
	}
	// Server
	if !cfg.Server.BasicAuth.Enabled || cfg.Server.BasicAuth.Username != "admin" {
		t.Errorf("server basic_auth 解析错误：%+v", cfg.Server.BasicAuth)
	}
}

// TestAutoApproveBool 验证 auto_approve 的布尔写法。
func TestAutoApproveBool(t *testing.T) {
	dir := t.TempDir()
	yml := `
mcp_servers:
  trusted:
    transport: stdio
    command: ./x
    auto_approve: true
`
	path := writeFile(t, dir, "config.yml", yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if !cfg.MCPServers["trusted"].AutoApprove.All {
		t.Errorf("auto_approve: true 应解析为 All=true，实际 %+v", cfg.MCPServers["trusted"].AutoApprove)
	}
}

// TestFileResolution 验证 @file: 系统提示词从文件读取。
func TestFileResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "system.md", "这是来自文件的系统提示词。")
	yml := `
chats:
  default:
    model: default
    system: "@file:./system.md"
`
	path := writeFile(t, dir, "config.yml", yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.Chats["default"].System != "这是来自文件的系统提示词。" {
		t.Errorf("@file: 解析错误：%q", cfg.Chats["default"].System)
	}
}

// TestEnvVarResolution 验证 ${VAR} 环境变量插值。
func TestEnvVarResolution(t *testing.T) {
	t.Setenv("TEST_API_KEY", "sk-12345")
	dir := t.TempDir()
	yml := `
providers:
  openai:
    type: openai
    api_key: ${TEST_API_KEY}
`
	path := writeFile(t, dir, "config.yml", yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.Providers["openai"].APIKey != "sk-12345" {
		t.Errorf("环境变量插值错误：%q", cfg.Providers["openai"].APIKey)
	}
}

// TestEnvVarUndefined 验证未定义的环境变量被替换为空字符串。
func TestEnvVarUndefined(t *testing.T) {
	os.Unsetenv("DEFINITELY_NOT_SET_VAR_XYZ")
	dir := t.TempDir()
	yml := `
providers:
  openai:
    type: openai
    api_key: ${DEFINITELY_NOT_SET_VAR_XYZ}
`
	path := writeFile(t, dir, "config.yml", yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.Providers["openai"].APIKey != "" {
		t.Errorf("未定义环境变量应为空，实际 %q", cfg.Providers["openai"].APIKey)
	}
}

// TestTemplateVariables 验证 system 中的 Go 模板变量被正确渲染。
func TestTemplateVariables(t *testing.T) {
	dir := t.TempDir()
	yml := `
chats:
  default:
    model: default
    system: |
      用户：{{.User}}
      主目录：{{.Home}}
      工作目录：{{.Cwd}}
      日期：{{.Date}}
`
	path := writeFile(t, dir, "config.yml", yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}

	sys := cfg.Chats["default"].System

	// 模板占位符应被替换，不再包含 {{ }}。
	if strings.Contains(sys, "{{") {
		t.Errorf("模板变量未被渲染：%q", sys)
	}

	// 校验关键变量渲染结果。
	wantDate := time.Now().Format("2006-01-02")
	if !strings.Contains(sys, wantDate) {
		t.Errorf("日期渲染错误，期望包含 %q，实际 %q", wantDate, sys)
	}
	if cwd, err := os.Getwd(); err == nil && !strings.Contains(sys, cwd) {
		t.Errorf("Cwd 渲染错误，期望包含 %q", cwd)
	}
	if home, err := os.UserHomeDir(); err == nil && !strings.Contains(sys, home) {
		t.Errorf("Home 渲染错误，期望包含 %q", home)
	}
	if u, err := user.Current(); err == nil && u.Username != "" && !strings.Contains(sys, u.Username) {
		t.Errorf("User 渲染错误，期望包含 %q", u.Username)
	}
}

// TestFileWithTemplate 验证 @file: 载入的内容同样会被模板渲染。
func TestFileWithTemplate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sys.md", "当前用户是 {{.User}}。")
	yml := `
chats:
  default:
    model: default
    system: "@file:./sys.md"
`
	path := writeFile(t, dir, "config.yml", yml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if strings.Contains(cfg.Chats["default"].System, "{{") {
		t.Errorf("@file 内容中的模板变量未渲染：%q", cfg.Chats["default"].System)
	}
}
