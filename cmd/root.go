// Package cmd 定义 chat-runtime 的命令行入口。
//
// 包含两个命令：
//   - 根命令（root.go）：CLI 交互模式 / 一次性任务模式；
//   - serve 子命令（serve.go）：WebSocket Web 服务模式。
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/spf13/cobra"
)

// 全局命令行标志。
var (
	// configPath 配置文件路径（--config / -f）。
	configPath string
	// chatName 使用的对话预设名称（--chat / -c）。
	chatName string
	// onceQuery 一次性任务模式的查询内容（--once）；非空表示单次执行后退出。
	onceQuery string
	// debug 是否开启调试输出（--debug）。
	debug bool
)

// AgentFactory 由上层装配（wiring）代码在初始化时注入，
// 根据配置与对话名创建一个已注册工具的 Agent，并返回用于释放资源的清理函数。
//
// 通过包级变量注入而非直接依赖具体装配逻辑，避免 cmd 包与 Provider/Tools 的
// 构造细节耦合，也便于在测试中替换为 mock。
type AgentFactory func(cfg *config.Config, chatName string) (ag *agent.Agent, cleanup func() error, err error)

// agentFactory 保存注入的工厂实现。
var agentFactory AgentFactory

// SetAgentFactory 供上层装配代码注册 Agent 工厂。
func SetAgentFactory(f AgentFactory) { agentFactory = f }

// rootCmd 是 CLI 的根命令，默认进入交互式 REPL。
var rootCmd = &cobra.Command{
	Use:   "chat-runtime",
	Short: "chat-runtime —— 轻量、可嵌入的 LLM Agent 运行时框架",
	Long: `chat-runtime 是一个基于 Go 的 LLM Agent 运行时框架。

支持多 Provider、MCP 工具集成、上下文管理与会话持久化，
提供 CLI 交互模式与 WebSocket Web 服务两种运行方式。`,
	// 关闭 cobra 自带的用法/错误重复打印，由我们统一处理。
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runRoot,
}

func init() {
	// 注册全局持久化标志，供根命令与所有子命令共享。
	pf := rootCmd.PersistentFlags()
	pf.StringVarP(&configPath, "config", "f", "config.yml", "配置文件路径")
	pf.StringVarP(&chatName, "chat", "c", "", "使用的对话预设名称（缺省为 default）")
	pf.BoolVar(&debug, "debug", false, "开启调试输出")

	// --once 仅在根命令（CLI 交互模式）下有意义。
	rootCmd.Flags().StringVar(&onceQuery, "once", "", "一次性任务模式：执行单次查询后退出")
}

// Execute 是 main 包调用的入口，执行根命令并处理错误退出码。
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// runRoot 执行根命令：加载配置、创建 Agent，然后进入一次性或交互式模式。
func runRoot(cmd *cobra.Command, args []string) error {
	// 加载配置（含环境变量插值与校验）。
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[debug] 已加载配置: %s\n", configPath)
	}

	// 校验对话预设存在。
	if _, err := cfg.Chat(chatName); err != nil {
		return err
	}

	// 通过工厂创建 Agent。
	if agentFactory == nil {
		return errors.New("AgentFactory 未初始化：请在启动时调用 cmd.SetAgentFactory 注册装配实现")
	}
	ag, cleanup, err := agentFactory(cfg, chatName)
	if err != nil {
		return fmt.Errorf("创建 Agent 失败: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// CLI 模式使用终端审批处理器：在终端提示用户 Y/N 确认。
	ag.SetApprovalHandler(agent.NewCLIApprovalHandler())

	// --once 一次性任务：执行单次查询后退出。
	if onceQuery != "" {
		return runOnce(cmd.Context(), ag, onceQuery)
	}

	// 否则进入交互式 REPL。
	return runREPL(ag)
}

// runOnce 执行一次性查询，将结果流式输出到标准输出。
func runOnce(ctx context.Context, ag *agent.Agent, query string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	err := ag.Run(ctx, query, streamToTerminal)
	fmt.Println() // 结束后补一个换行，避免与后续输出粘连。
	return err
}

// runREPL 进入交互式读取-求值-打印循环（REPL）。
//
// 关于 Ctrl+C 的处理：在生成过程中按 Ctrl+C 只取消“当前这一次生成”，
// 而不是退出整个程序；空闲状态下再次按 Ctrl+C（或输入 /quit）才退出。
func runREPL(ag *agent.Agent) error {
	printBanner()

	scanner := bufio.NewScanner(os.Stdin)
	// 允许较长的单行输入（默认 64KB 可能不够）。
	const maxLine = 1 << 20 // 1MB
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	for {
		fmt.Print("\n\033[1;36m你 >\033[0m ")
		if !scanner.Scan() {
			// EOF（Ctrl+D）或读取错误：优雅退出。
			fmt.Println("\n再见 👋")
			return scanner.Err()
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// 处理斜杠命令。
		if strings.HasPrefix(line, "/") {
			if quit := handleSlashCommand(line, ag); quit {
				fmt.Println("再见 👋")
				return nil
			}
			continue
		}

		// 普通输入：执行一次生成，期间支持 Ctrl+C 取消当前生成。
		runInteractiveTurn(ag, line)
	}
}

// runInteractiveTurn 执行一轮交互生成，并在此期间接管 SIGINT（Ctrl+C）以取消当前生成。
func runInteractiveTurn(ag *agent.Agent, input string) {
	// 为本轮生成创建可取消的 context。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 仅在本轮生成期间监听 SIGINT，用于取消当前生成而非退出程序。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-sigCh:
			fmt.Print("\n\033[33m[已中断当前生成]\033[0m\n")
			cancel()
		case <-ctx.Done():
			// 本轮正常结束，退出监听 goroutine。
		}
	}()

	fmt.Print("\033[1;32m助手 >\033[0m ")
	if err := ag.Run(ctx, input, streamToTerminal); err != nil {
		if errors.Is(err, context.Canceled) {
			// 用户主动取消，不视为错误。
			return
		}
		fmt.Fprintf(os.Stderr, "\n\033[31m错误: %v\033[0m\n", err)
		return
	}
	fmt.Println()
}

// streamToTerminal 将 Agent 事件渲染到终端。
func streamToTerminal(ev agent.Event) {
	switch ev.Type {
	case agent.EventToken:
		// 文本增量：直接输出，不换行。
		fmt.Print(ev.Content)
	case agent.EventToolCall:
		// 工具调用：以醒目颜色提示。
		fmt.Printf("\n\033[35m[调用工具] %s %v\033[0m\n", ev.ToolName, ev.ToolArgs)
	case agent.EventToolResult:
		fmt.Printf("\033[90m[工具结果] %s: %s\033[0m\n", ev.ToolName, truncate(ev.ToolResult, 500))
	case agent.EventError:
		fmt.Fprintf(os.Stderr, "\n\033[31m[错误] %s\033[0m\n", ev.Content)
	case agent.EventDone:
		if debug && ev.Usage != nil {
			fmt.Fprintf(os.Stderr, "\n\033[90m[用量] prompt=%d completion=%d total=%d\033[0m\n",
				ev.Usage.PromptTokens, ev.Usage.CompletionTokens, ev.Usage.TotalTokens)
		}
	}
}

// handleSlashCommand 处理斜杠命令，返回 true 表示应退出程序。
func handleSlashCommand(line string, ag *agent.Agent) (quit bool) {
	// 只取第一个词作为命令。
	cmd := strings.Fields(line)[0]
	switch cmd {
	case "/help", "/h":
		printHelp()
	case "/clear":
		ag.Clear()
		fmt.Println("\033[90m已清空会话历史。\033[0m")
	case "/history":
		printHistory(ag.History())
	case "/tools":
		printTools(ag.ToolNames())
	case "/quit", "/exit", "/q":
		return true
	default:
		fmt.Printf("\033[31m未知命令: %s（输入 /help 查看帮助）\033[0m\n", cmd)
	}
	return false
}

// printBanner 打印启动横幅。
func printBanner() {
	fmt.Println("\033[1;36mchat-runtime\033[0m 交互模式")
	fmt.Println("输入内容开始对话，输入 \033[1m/help\033[0m 查看可用命令，\033[1m/quit\033[0m 退出。")
	fmt.Println("生成过程中按 \033[1mCtrl+C\033[0m 可中断当前生成（不会退出程序）。")
}

// printHelp 打印斜杠命令帮助。
func printHelp() {
	fmt.Println("可用命令：")
	fmt.Println("  /help     显示本帮助")
	fmt.Println("  /clear    清空当前会话历史")
	fmt.Println("  /history  查看会话历史")
	fmt.Println("  /tools    列出当前对话可用的工具")
	fmt.Println("  /quit     退出程序")
}

// printHistory 打印会话历史。
func printHistory(msgs []agent.Message) {
	if len(msgs) == 0 {
		fmt.Println("\033[90m（暂无历史消息）\033[0m")
		return
	}
	for _, m := range msgs {
		fmt.Printf("\033[1m[%s]\033[0m %s\n", m.Role, m.Content)
	}
}

// printTools 打印可用工具列表。
func printTools(tools []string) {
	if len(tools) == 0 {
		fmt.Println("\033[90m（当前对话未启用任何工具）\033[0m")
		return
	}
	fmt.Println("可用工具：")
	for _, t := range tools {
		fmt.Printf("  - %s\n", t)
	}
}

// truncate 将字符串截断到最多 n 个字符，超出部分以省略号表示。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
