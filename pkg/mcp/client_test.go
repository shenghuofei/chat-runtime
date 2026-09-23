package mcp

import (
	"sync"
	"testing"

	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/mark3labs/mcp-go/mcp"
)

// newClientForLock 构造一个仅用于测试并发锁选择逻辑的 MCPClient（不建立真实连接）。
//
// 它复刻 NewMCPClient 中与并发控制相关的字段初始化：为 no_concurrent_tools
// 中的每个工具预分配工具级锁，并记录 no_concurrent 标志。
func newClientForLock(cfg config.MCPServerConfig) *MCPClient {
	toolMu := make(map[string]*sync.Mutex, len(cfg.NoConcurrentTools))
	for _, name := range cfg.NoConcurrentTools {
		toolMu[name] = &sync.Mutex{}
	}
	return &MCPClient{
		config:       cfg,
		toolMu:       toolMu,
		noConcurrent: cfg.NoConcurrent,
	}
}

// TestToolNamePrefixing 验证工具名按 `<server>__<tool>` 前缀命名。
func TestToolNamePrefixing(t *testing.T) {
	c := &MCPClient{config: config.MCPServerConfig{Name: "k8s-eye"}}

	got := c.prefixedName("get_pods")
	want := "k8s-eye__get_pods"
	if got != want {
		t.Errorf("前缀命名错误：want=%q got=%q", want, got)
	}

	// 通过 newMCPTool 验证 Name() 与 Schema().Name 一致带前缀。
	tool := c.newMCPTool(mcp.Tool{Name: "describe_pod", Description: "描述 Pod"})
	if tool.Name() != "k8s-eye__describe_pod" {
		t.Errorf("MCPTool.Name 前缀错误：%q", tool.Name())
	}
	if tool.Schema().Name != "k8s-eye__describe_pod" {
		t.Errorf("Schema.Name 前缀错误：%q", tool.Schema().Name)
	}
	// rawName 应保持不含前缀。
	if tool.rawName != "describe_pod" {
		t.Errorf("rawName 不应含前缀：%q", tool.rawName)
	}
}

// TestAutoApproveAll 验证 auto_approve: true 时所有工具自动批准。
func TestAutoApproveAll(t *testing.T) {
	c := &MCPClient{config: config.MCPServerConfig{
		Name:        "trusted",
		AutoApprove: config.AutoApprove{All: true},
	}}

	if !c.isAutoApproved("any_tool") {
		t.Error("All=true 时任意工具都应自动批准")
	}
	tool := c.newMCPTool(mcp.Tool{Name: "any_tool"})
	if tool.NeedApproval() {
		t.Error("自动批准的工具 NeedApproval 应为 false")
	}
}

// TestAutoApproveList 验证 auto_approve 列表：仅列表内工具自动批准。
func TestAutoApproveList(t *testing.T) {
	c := &MCPClient{config: config.MCPServerConfig{
		Name:        "k8s-eye",
		AutoApprove: config.AutoApprove{Tools: []string{"get_pods", "describe_pod"}},
	}}

	if !c.isAutoApproved("get_pods") {
		t.Error("get_pods 在列表内，应自动批准")
	}
	if c.isAutoApproved("delete_pod") {
		t.Error("delete_pod 不在列表内，不应自动批准")
	}

	// 列表内工具 NeedApproval=false，列表外工具 NeedApproval=true。
	safe := c.newMCPTool(mcp.Tool{Name: "get_pods"})
	if safe.NeedApproval() {
		t.Error("列表内工具 NeedApproval 应为 false")
	}
	dangerous := c.newMCPTool(mcp.Tool{Name: "delete_pod"})
	if !dangerous.NeedApproval() {
		t.Error("列表外工具 NeedApproval 应为 true")
	}
}

// TestAutoApproveNone 验证未配置 auto_approve 时全部需审批。
func TestAutoApproveNone(t *testing.T) {
	c := &MCPClient{config: config.MCPServerConfig{Name: "dangerous-ops"}}
	if c.isAutoApproved("anything") {
		t.Error("未配置 auto_approve 时不应自动批准")
	}
}

// TestShouldIncludeWhitelist 验证 include 白名单过滤。
func TestShouldIncludeWhitelist(t *testing.T) {
	c := &MCPClient{config: config.MCPServerConfig{
		Name:    "data-api",
		Include: []string{"query_metric", "list_dashboards"},
	}}

	if !c.shouldInclude("query_metric") {
		t.Error("白名单内工具应通过")
	}
	if c.shouldInclude("delete_dashboard") {
		t.Error("白名单外工具应被过滤")
	}
}

// TestShouldIncludeBlacklist 验证 exclude 黑名单过滤。
func TestShouldIncludeBlacklist(t *testing.T) {
	c := &MCPClient{config: config.MCPServerConfig{
		Name:    "k8s-eye",
		Exclude: []string{"delete_namespace", "drain_node"},
	}}

	if c.shouldInclude("delete_namespace") {
		t.Error("黑名单内工具应被过滤")
	}
	if !c.shouldInclude("get_pods") {
		t.Error("黑名单外工具应通过")
	}
}

// TestLockForConcurrency 验证并发控制锁的选择逻辑。
func TestLockForConcurrency(t *testing.T) {
	// Server 级串行：所有工具返回同一把（非 nil）锁。
	serverLevel := newClientForLock(config.MCPServerConfig{
		Name:         "stateful",
		NoConcurrent: true,
	})
	if serverLevel.lockFor("tool_a") == nil || serverLevel.lockFor("tool_b") == nil {
		t.Error("no_concurrent=true 时所有工具都应有锁")
	}
	if serverLevel.lockFor("tool_a") != serverLevel.lockFor("tool_b") {
		t.Error("Server 级串行应共用同一把锁")
	}

	// 工具级串行：仅列表内工具有锁，其余为 nil。
	toolLevel := newClientForLock(config.MCPServerConfig{
		Name:              "mixed",
		NoConcurrentTools: []string{"write_config"},
	})
	if toolLevel.lockFor("write_config") == nil {
		t.Error("no_concurrent_tools 内的工具应有锁")
	}
	if toolLevel.lockFor("read_config") != nil {
		t.Error("列表外工具应可并发（无锁）")
	}
}
