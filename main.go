package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shenghuofei/chat-runtime/cmd"
	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/logging"
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

// globalFileStore 跨会话共享的 FileStore 单例。
// 使用 sync.Once 延迟初始化：CLI 模式下只调用一次 buildAgent，
// Web 模式下每个连接都会调用 buildAgent，若每次都 os.MkdirAll + 创建 FileStore
// 则存在不必要的重复文件系统操作，用单例避免之。
var (
	globalFileStore     *store.FileStore
	globalFileStoreOnce sync.Once
	globalFileStoreErr  error
	// globalHomeDir 由 getGlobalFileStore 在首次初始化时写入，
	// 供同一 buildAgent 调用中的 stableSessionID 直接复用，避免重复 syscall。
	globalHomeDir string
)

// getGlobalFileStore 返回全局共享的 FileStore，首次调用时初始化。
// 同时将用户主目录写入 globalHomeDir，供调用方复用。
func getGlobalFileStore() (*store.FileStore, error) {
	globalFileStoreOnce.Do(func() {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			globalFileStoreErr = fmt.Errorf("获取用户主目录失败: %w", err)
			return
		}
		globalHomeDir = homeDir
		storeDir := filepath.Join(homeDir, ".chat-runtime", "sessions")
		globalFileStore, globalFileStoreErr = store.NewFileStore(storeDir)
	})
	return globalFileStore, globalFileStoreErr
}

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

	// 初始化结构化日志。
	// 日志级别和格式通过环境变量控制：
	//   LOG_LEVEL=debug|info|warn|error（默认 info）
	//   LOG_FORMAT=json|text（默认 text）
	logging.Init(os.Getenv("LOG_LEVEL"), os.Getenv("LOG_FORMAT"))

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
	// 注入 Store 获取函数，供 serve 子命令的会话管理 API 使用。
	cmd.SetStoreGetter(func() (store.Store, error) {
		return getGlobalFileStore()
	})
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
	// 优化：对 apiKey 做 SHA256 哈希后取前 16 字符作为缓存 key，
	// 避免缓存 key 包含明文密钥，防止意外泄露。
	keyHash := hashKey(providerCfg.APIKey)
	headersHash := hashHeaders(providerCfg.ExtraHeaders)
	cacheKey := fmt.Sprintf("%s:%s:%s:%s:%s", providerCfg.Type, modelCfg.Model, providerCfg.BaseURL, keyHash, headersHash)
	providerFactory := func() (provider.Provider, error) {
		globalProviderCacheMu.Lock()
		defer globalProviderCacheMu.Unlock()
		if p, ok := globalProviderCache[cacheKey]; ok {
			return p, nil
		}
		p, err := provider.New(provider.ProviderConfig{
			Type:         providerCfg.Type,
			APIKey:       providerCfg.APIKey,
			BaseURL:      providerCfg.BaseURL,
			Model:        modelCfg.Model,
			ExtraHeaders: providerCfg.ExtraHeaders,
		})
		if err != nil {
			return nil, err
		}
		globalProviderCache[cacheKey] = p
		return p, nil
	}

	// 4. 获取全局共享的 FileStore（懒初始化单例，所有会话共用同一实例）。
	fileStore, err := getGlobalFileStore()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Store 失败: %w", err)
	}

	// 5. 创建 Session（会自动创建 Provider + Manager + Agent）
	// 使用稳定的 Session ID（基于 chatName + 用户主目录的哈希），
	// 使重启后能恢复到同一会话的历史。
	// globalHomeDir 已由 getGlobalFileStore 的 sync.Once 写入，无需重复调用 syscall。
	sessionID := stableSessionID(chatName, globalHomeDir)
	session, err := agent.NewSession(sessionID, chatName, chatCfg, providerFactory, fileStore)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Session 失败: %w", err)
	}

	// 6. 注册内置工具
	registerBuiltinTools(cfg, session.Agent)

	// 6.1 将 ModelConfig 中的采样参数传递给 Provider，使配置真正生效。
	// 仅在配置了有效值（>0）时才追加对应选项，未配置则由 Provider 使用默认值。
	var providerOpts []provider.Option
	if modelCfg.Temperature > 0 {
		providerOpts = append(providerOpts, provider.WithTemperature(modelCfg.Temperature))
	}
	if modelCfg.MaxTokens > 0 {
		providerOpts = append(providerOpts, provider.WithMaxTokens(modelCfg.MaxTokens))
	}
	if modelCfg.TopP > 0 {
		providerOpts = append(providerOpts, provider.WithTopP(modelCfg.TopP))
	}
	session.Agent.SetProviderOptions(providerOpts...)

	// 7. 连接 MCP Server，发现并注册远程工具
	mcpClients := connectMCPServers(cfg, chatCfg, session.Agent)

	// 8. 构建清理函数
	cleanup := func() error {
		// 持久化会话历史
		if err := session.Persist(); err != nil {
			slog.Warn("持久化会话失败", "error", err)
		}
		// 释放 Store 的写入器与 sync.Map 条目（保留磁盘文件）。
		// Web 模式每连接随机 sessionID，不释放会导致句柄/内存泄漏。
		fileStore.Release(context.Background(), sessionID)
		// 关闭 MCP 客户端
		for _, c := range mcpClients {
			if err := c.Close(); err != nil {
				slog.Warn("关闭 MCP 客户端失败", "error", err)
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
			AutoApprove:     cfg.Tools.Command.AutoApprove.All,
		})
		ag.RegisterTool(cmdTool)
	}
	if cfg.Tools.Filesystem.Enabled {
		slog.Warn("filesystem 工具已在配置中启用，但当前版本尚未实现该工具，配置将被忽略")
	}
}

// connectMCPServers 按配置并发连接所有 MCP Server，发现工具后注册到 Agent。
// 每个 Server 在独立 goroutine 中初始化，互不阻塞；单个失败不影响其他 Server。
// 返回已成功建立连接的客户端列表（供 cleanup 关闭）。
func connectMCPServers(cfg *config.Config, chatCfg config.ChatConfig, ag *agent.Agent) []*mcp.MCPClient {
	// 先收集需要初始化的 Server，过滤掉不在白名单内的。
	type target struct {
		name string
		cfg  config.MCPServerConfig
	}
	var targets []target
	for name, mcpCfg := range cfg.MCPServers {
		if len(chatCfg.MCPServers) > 0 && !contains(chatCfg.MCPServers, name) {
			continue
		}
		targets = append(targets, target{name, mcpCfg})
	}
	if len(targets) == 0 {
		return nil
	}

	// 并发初始化：每个 Server 一个 goroutine，结果通过有缓冲 channel 回收。
	type result struct {
		name   string
		client *mcp.MCPClient
		tools  []agent.Tool
	}
	resultCh := make(chan result, len(targets))

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(name string, mcpCfg config.MCPServerConfig) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), mcpInitTimeout)
			defer cancel()
			client, mcpTools, err := initMCPServer(ctx, name, mcpCfg)
			if err != nil {
				slog.Warn("MCP Server 初始化失败，跳过", "server", name, "error", err)
				return
			}
			resultCh <- result{name: name, client: client, tools: mcpTools}
		}(t.name, t.cfg)
	}

	// 等所有 goroutine 完成后关闭 channel，让下面的 range 能正常退出。
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// 在主 goroutine 中顺序消费结果并注册工具（RegisterTool 非并发安全）。
	var mcpClients []*mcp.MCPClient
	for r := range resultCh {
		mcpClients = append(mcpClients, r.client)
		for _, t := range r.tools {
			ag.RegisterTool(t)
		}
		slog.Info("已连接 MCP Server", "server", r.name, "tools", len(r.tools))
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

// hashKey 对密钥做 SHA256 哈希并取前 16 个十六进制字符，
// 用于 Provider 缓存的 key，避免明文密钥出现在内存映射中。
func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])[:16]
}

// hashHeaders 对请求头 map 做确定性哈希（先按 key 排序再序列化），
// 避免 map 遍历随机顺序导致同一配置生成不同缓存 key。
func hashHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return hashKey("")
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(headers[k])
		b.WriteByte(';')
	}
	return hashKey(b.String())
}

// stableSessionID 基于 chatName 与用户主目录生成稳定的 Session ID。
//
// 使用 SHA256 哈希取前 8 字符作为后缀，保证：
//   - 同一用户、同一 chatName 始终映射到同一 Session（跨重启可恢复历史）；
//   - 不同用户或不同 chatName 产生不同的 Session。
//
// homeDir 由调用方传入，避免重复调用 os.UserHomeDir()。
func stableSessionID(chatName, homeDir string) string {
	raw := fmt.Sprintf("%s:%s", chatName, homeDir)
	h := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(h[:])[:8]
	return fmt.Sprintf("%s_%s", chatName, suffix)
}
