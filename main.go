package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/shenghuofei/chat-runtime/cmd"
	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/mcp"
	"github.com/shenghuofei/chat-runtime/pkg/provider"
	"github.com/shenghuofei/chat-runtime/pkg/store"
	"github.com/shenghuofei/chat-runtime/pkg/tools"
)

var (
	Version   = "dev"
	BuildTime = "unknown"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("chat-runtime %s\n", Version)
		fmt.Printf("Build Time: %s\n", BuildTime)
		fmt.Printf("Go Version: %s\n", runtime.Version())
		fmt.Printf("Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		return
	}

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

	// 3. 创建 Provider 工厂函数
	providerFactory := func(cc config.ChatConfig) (provider.Provider, error) {
		return provider.New(provider.ProviderConfig{
			Type:    providerCfg.Type,
			APIKey:  providerCfg.APIKey,
			BaseURL: providerCfg.BaseURL,
			Model:   modelCfg.Model,
		})
	}

	// 4. 创建 Store（持久化）
	homeDir, _ := os.UserHomeDir()
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
		session.Agent.RegisterTool(cmdTool)
	}

	// 7. 连接 MCP Server，发现并注册远程工具
	var mcpClients []*mcp.MCPClient
	for name, mcpCfg := range cfg.MCPServers {
		// 如果对话配置了 mcp_servers 白名单，只连接指定的
		if len(chatCfg.MCPServers) > 0 && !contains(chatCfg.MCPServers, name) {
			continue
		}

		client, err := mcp.NewMCPClient(mcpCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 连接 MCP Server %q 失败: %v（跳过）\n", name, err)
			continue
		}
		mcpClients = append(mcpClients, client)

		mcpTools, err := client.DiscoverTools()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 从 MCP Server %q 发现工具失败: %v（跳过）\n", name, err)
			continue
		}
		for _, t := range mcpTools {
			session.Agent.RegisterTool(t)
		}
		fmt.Fprintf(os.Stderr, "[MCP] 已连接 %s，注册了 %d 个工具\n", name, len(mcpTools))
	}

	// 8. 构建清理函数
	cleanup := func() error {
		// 先持久化会话
		if err := session.Persist(); err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 持久化会话失败: %v\n", err)
		}
		// 关闭 MCP 客户端
		for _, c := range mcpClients {
			c.Close()
		}
		// 关闭 Session（含 Provider）
		return session.Close()
	}

	return session.Agent, cleanup, nil
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
