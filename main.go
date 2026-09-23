package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/shenghuofei/chat-runtime/cmd"
	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/mcp"
	"github.com/shenghuofei/chat-runtime/pkg/provider"
	"github.com/shenghuofei/chat-runtime/pkg/store"
	"github.com/shenghuofei/chat-runtime/pkg/tools"
)

// globalProviderCache 跨会话共享的 Provider 实例缓存，避免重复创建 HTTP 连接池。
// key 格式："{type}:{model}:{baseURL}:{apiKey}"
var (
	globalProviderCache   = make(map[string]provider.Provider)
	globalProviderCacheMu sync.Mutex
)

var (
	Version   = "dev"
	BuildTime = "unknown"
)

// mcpInitTimeout 是单个 MCP Server 初始化（连接 + 握手 + 工具发现）的超时上限。
const mcpInitTimeout = 30 * time.Second

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("chat-runtime %s\n", Version)
		fmt.Printf("Build Time: %s\n", BuildTime)
		fmt.Printf("Go Version: %s\n", runtime.Version())
		fmt.Printf("Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		return
	}

	// 进程退出时统一关闭所有缓存的 Provider，释放 HTTP 连接。
	defer func() {
		globalProviderCacheMu.Lock()
		defer globalProviderCacheMu.Unlock()
		for _, p := range globalProviderCache {
			_ = p.Close()
		}
	}()

	// 注入 Agent 工厂：把 config → provider → manager → agent 的装配逻辑串起来。
	cmd.SetAgentFactory(buildAgent)
	cmd.Execute()
}

// buildAgent 是 AgentFactory 的实现：根据配置和对话名创建一个完整的 Agent。
//
// 装配流程：
//  1. 解析对话配置，找到关联的 model 和 provider
//  2. 创建 LLM Provider 实例
//  3. 创建 Session（内含 Manager + Store + Agent）
//  4. 注册内置工具（命令执行等）
//  5. 连接 MCP Server，发现并注册远程工具
//  6. 返回 Agent 和清理函数
func buildAgent(cfg *config.Config, chatName string) (*agent.Agent, func() error, error) {
	// 1. 获取对话配置
	chatCfg, err := cfg.Chat(chatName)
	if err != nil {
		return nil, nil, err
	}

	// 2. 解析 model → provider 链
	modelCfg, ok := cfg.Models[chatCfg.Model]
	if !ok {
		return nil, nil, fmt.Errorf("对话 %q 引用了不存在的模型 %q", chatName, chatCfg.Model)
	}
	providerCfg, ok := cfg.Providers[modelCfg.Provider]
	if !ok {
		return nil, nil, fmt.Errorf("模型 %q 引用了不存在的 provider %q", chatCfg.Model, modelCfg.Provider)
	}

	// 3. 创建 Provider 工厂函数（带缓存：相同配置的 Provider 只创建一次，共享 HTTP 连接池）
	cacheKey := fmt.Sprintf("%s:%s:%s:%s", providerCfg.Type, modelCfg.Model, providerCfg.BaseURL, providerCfg.APIKey)
	providerFactory := func(cc config.ChatConfig) (provider.Provider, error) {
		globalProviderCacheMu.Lock()
		defer globalProviderCacheMu.Unlock()
		if p, ok := globalProviderCache[cacheKey]; ok {
			return p, nil
		}
		p, err := provider.New(provider.ProviderConfig{
			Type:    providerCfg.Type,
			APIKey:  providerCfg.APIKey,
			BaseURL: providerCfg.BaseURL,
			Model:   modelCfg.Model,
		})
		if err != nil {
			return nil, err
		}
		globalProviderCache[cacheKey] = p
		return p, nil
	}

	// 4. 创建 Store（持久化）
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("获取用户主目录失败: %w", err)
	}
	storeDir := filepath.Join(homeDir, ".chat-runtime", "sessions")
	fileStore, err := store.NewFileStore(storeDir)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Store 失败: %w", err)
	}

	// 5. 创建 Session（会自动创建 Provider + Manager + Agent）
	sessionID := fmt.Sprintf("%s_%d", chatName, os.Getpid())
	session, err := agent.NewSession(sessionID, chatName, chatCfg, providerFactory, fileStore)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Session 失败: %w", err)
	}

	// 6. 注册内置工具
	registerBuiltinTools(cfg, session.Agent)

	// 7. 连接 MCP Server，发现并注册远程工具
	mcpClients := connectMCPServers(cfg, chatCfg, session.Agent)

	// 8. 构建清理函数
	cleanup := func() error {
		// 持久化会话历史
		if err := session.Persist(); err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 持久化会话失败: %v\n", err)
		}
		// 关闭 MCP 客户端
		for _, c := range mcpClients {
			if err := c.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "[警告] 关闭 MCP 客户端失败: %v\n", err)
			}
		}
		// Provider 已缓存，不在此处关闭；main() 退出时统一清理。
		return nil
	}

	return session.Agent, cleanup, nil
}

// registerBuiltinTools 将配置中启用的内置工具注册到 Agent。
func registerBuiltinTools(cfg *config.Config, ag *agent.Agent) {
	if cfg.Tools.Command.Enabled {
		var timeout time.Duration
		if cfg.Tools.Command.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Tools.Command.Timeout); err == nil {
				timeout = d
			}
		}
		cmdTool := tools.NewCommandTool(tools.CommandToolConfig{
			AllowedCommands: cfg.Tools.Command.Whitelist,
			Timeout:         timeout,
		})
		ag.RegisterTool(cmdTool)
	}
}

// connectMCPServers 按配置连接所有 MCP Server，发现工具后注册到 Agent。
// 每个 Server 独立使用带超时的 context，单个失败不影响其他 Server。
// 返回已成功建立连接的客户端列表（供 cleanup 关闭）。
func connectMCPServers(cfg *config.Config, chatCfg config.ChatConfig, ag *agent.Agent) []*mcp.MCPClient {
	var mcpClients []*mcp.MCPClient

	for name, mcpCfg := range cfg.MCPServers {
		// 如果对话配置了 mcp_servers 白名单，只连接指定的
		if len(chatCfg.MCPServers) > 0 && !contains(chatCfg.MCPServers, name) {
			continue
		}

		// 每个 MCP Server 使用独立的带超时 context，防止单个卡顿阻塞全部
		ctx, cancel := context.WithTimeout(context.Background(), mcpInitTimeout)
		client, mcpTools, err := initMCPServer(ctx, name, mcpCfg)
		cancel()

		if err != nil {
			fmt.Fprintf(os.Stderr, "[警告] MCP Server %q 初始化失败: %v（跳过）\n", name, err)
			continue
		}

		mcpClients = append(mcpClients, client)
		for _, t := range mcpTools {
			ag.RegisterTool(t)
		}
		fmt.Fprintf(os.Stderr, "[MCP] 已连接 %s，注册了 %d 个工具\n", name, len(mcpTools))
	}

	return mcpClients
}

// initMCPServer 连接单个 MCP Server 并发现其工具。ctx 用于控制超时。
func initMCPServer(ctx context.Context, name string, mcpCfg config.MCPServerConfig) (*mcp.MCPClient, []agent.Tool, error) {
	client, err := mcp.NewMCPClient(ctx, mcpCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("连接失败: %w", err)
	}

	mcpTools, err := client.DiscoverTools(ctx)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("工具发现失败: %w", err)
	}

	return client, mcpTools, nil
}

// contains 检查字符串切片是否包含目标值。
func contains(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
