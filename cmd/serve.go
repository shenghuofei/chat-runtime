package cmd

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/server"
	"github.com/shenghuofei/chat-runtime/pkg/store"
	"github.com/spf13/cobra"
)

// serve 子命令相关标志。
var (
	// servePort 监听端口（--port）。
	servePort int
	// serveHost 监听地址（--host）。
	serveHost string
	// serveBasicAuth 是否强制启用 Basic Auth（--basic-auth）。
	serveBasicAuth bool
)

// serveCmd 是 Web 服务模式子命令。
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "启动 WebSocket Web 服务模式",
	Long: `启动 chat-runtime 的 Web 服务模式。

前端静态资源已通过 go:embed 内嵌进二进制，无需额外部署。
可通过 --port / --host 覆盖配置文件中的 server 段；命令行参数优先级更高。`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runServe,
}

func init() {
	// 注册 serve 专属标志。
	f := serveCmd.Flags()
	f.IntVar(&servePort, "port", 8080, "监听端口")
	f.StringVar(&serveHost, "host", "0.0.0.0", "监听地址")
	f.BoolVar(&serveBasicAuth, "basic-auth", false, "强制启用 HTTP Basic Auth")

	// 挂载到根命令。
	rootCmd.AddCommand(serveCmd)
}

// runServe 加载配置、组装 Server 并启动监听。
func runServe(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// 命令行参数覆盖配置文件（仅当显式传入时覆盖）。
	f := cmd.Flags()
	if f.Changed("port") {
		cfg.Server.Port = servePort
	}
	if f.Changed("host") {
		cfg.Server.Host = serveHost
	}
	if f.Changed("basic-auth") && serveBasicAuth {
		cfg.Server.BasicAuth.Enabled = true
	}

	// 安全提示：公网暴露却未启用鉴权时给出告警。
	if !cfg.Server.BasicAuth.Enabled {
		fmt.Println("\033[33m[警告] 未启用 Basic Auth：Web 模式会让远程用户触发工具执行，公网暴露前请启用鉴权。\033[0m")
	}

	if agentFactory == nil {
		return errors.New("AgentFactory 未初始化：请在启动时调用 cmd.SetAgentFactory 注册装配实现")
	}

	// 获取全局 Store 供 Server 的会话管理 API 使用。
	var st store.Store
	if storeGetter != nil {
		if s, err := storeGetter(); err != nil {
			slog.Warn("Store 初始化失败，会话管理 API 不可用", "error", err)
		} else {
			st = s
		}
	}

	// 组装并启动 Server。cmd 与 server 的工厂签名一致，直接转换类型传入。
	srv := server.NewServer(cfg, server.AgentFactory(agentFactory), st)
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	return srv.Start(addr)
}
