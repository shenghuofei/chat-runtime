// Package cmd 定义 chat-runtime 的命令行入口。
//
// 包含两个命令：
//   - 根命令（root.go）：CLI 交互模式 / 一次性任务模式；
//   - serve 子命令（serve.go）：WebSocket Web 服务模式。
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chzyer/readline"
	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/store"
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
// 根据配置、对话名与会话 ID 创建一个已注册工具的 Agent，并返回用于释放资源的清理函数。
//
// sessionID 为空时由工厂自行决定 ID（CLI 模式使用稳定哈希 ID 以跨重启恢复历史，
// Web 新会话使用随机 ID）；非空时工厂使用该 ID 加载对应历史（Web Resume 场景）。
//
// 通过包级变量注入而非直接依赖具体装配逻辑，避免 cmd 包与 Provider/Tools 的
// 构造细节耦合，也便于在测试中替换为 mock。
type AgentFactory func(cfg *config.Config, chatName string, sessionID string) (ag *agent.Agent, cleanup func() error, err error)

// agentFactory 保存注入的工厂实现。
var agentFactory AgentFactory

// SetAgentFactory 供上层装配代码注册 Agent 工厂。
func SetAgentFactory(f AgentFactory) { agentFactory = f }

// StoreGetter 由上层注入，返回全局共享的 Store 实例。
// serve 子命令用它获取 Store 传给 Server 的会话管理 API。
type StoreGetter func() (store.Store, error)

var storeGetter StoreGetter

// SetStoreGetter 供上层注册 Store 获取函数。
func SetStoreGetter(f StoreGetter) { storeGetter = f }

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
	// CLI 模式传入空字符串，由 buildAgent 基于 chatName + 主目录生成稳定 ID，
	// 保证跨重启能恢复同一会话的历史。
	ag, cleanup, err := agentFactory(cfg, chatName, "")
	if err != nil {
		return fmt.Errorf("创建 Agent 失败: %w", err)
	}
	// 确保无论以何种方式退出（正常 /quit、Ctrl+D、panic）都执行资源清理：
	// 持久化会话历史、关闭 MCP 客户端等。
	if cleanup != nil {
		defer func() {
			if err := cleanup(); err != nil {
				fmt.Fprintf(os.Stderr, "[警告] 清理资源时出错: %v\n", err)
			}
		}()
	}

	// 注册 SIGTERM 信号处理：不直接 os.Exit（会跳过 defer 导致会话未持久化），
	// 而是通过 quitCh 通知主循环（REPL）优雅退出，从而正常执行 cleanup。
	quitCh := make(chan struct{})
	sigCleanup := make(chan os.Signal, 1)
	signal.Notify(sigCleanup, syscall.SIGTERM)
	go func() {
		<-sigCleanup
		fmt.Fprintln(os.Stderr, "\n收到 SIGTERM，正在清理资源...")
		// 通知主循环退出，由 runREPL 感知后返回，触发 defer 链（cleanup）。
		close(quitCh)
	}()
	defer signal.Stop(sigCleanup)

	// CLI 模式使用终端审批处理器：在终端提示用户 Y/N 确认。
	ag.SetApprovalHandler(agent.NewCLIApprovalHandler())

	// --once 一次性任务：执行单次查询后退出。
	if onceQuery != "" {
		return runOnce(cmd.Context(), ag, onceQuery)
	}

	// 否则进入交互式 REPL。
	return runREPL(ag, quitCh)
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
// 关于 Ctrl+C 的处理：在生成过程中按 Ctrl+C 只取消"当前这一次生成"，
// 而不是退出整个程序；空闲状态下按 Ctrl+C 或 Ctrl+D（或输入 /quit）退出。
func runREPL(ag *agent.Agent, quitCh <-chan struct{}) error {
	printBanner()

	// 历史记录文件：存放在 ~/.chat-runtime/history。
	// 目录以 0o700（仅属主可读写执行）创建，防止其他用户读取含敏感内容的历史记录。
	homeDir, _ := os.UserHomeDir()
	historyDir := filepath.Join(homeDir, ".chat-runtime")
	_ = os.MkdirAll(historyDir, 0o700)
	historyFile := filepath.Join(historyDir, "history")

	rl, err := readline.NewEx(&readline.Config{
		Prompt:            "\033[1;36m你 >\033[0m ",
		HistoryFile:       historyFile,
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true, // 历史搜索不区分大小写
	})
	if err != nil {
		return fmt.Errorf("初始化 readline 失败: %w", err)
	}
	defer rl.Close()

	// readline 的 Readline() 是阻塞调用，无法直接 select quitCh。
	// 因此将其放入独立 goroutine，通过 channel 将结果送回主循环，
	// 主循环同时监听 quitCh（SIGTERM 触发）以便优雅退出并执行 cleanup。
	type readResult struct {
		line string
		err  error
	}

	for {
		lineCh := make(chan readResult, 1)
		go func() {
			line, err := rl.Readline()
			lineCh <- readResult{line: line, err: err}
		}()

		var res readResult
		select {
		case <-quitCh:
			// 收到 SIGTERM：关闭 readline 使 Readline() goroutine 收到 io.EOF 并退出，
			// 防止 goroutine 永久阻塞在终端 I/O（defer rl.Close() 也会调用，幂等安全）。
			rl.Close()
			fmt.Println("\n再见 👋")
			return nil
		case res = <-lineCh:
		}

		if res.err != nil {
			if res.err == readline.ErrInterrupt || res.err == io.EOF {
				// Ctrl+C 或 Ctrl+D：优雅退出。
				fmt.Println("\n再见 👋")
				return nil
			}
			return res.err
		}

		line := strings.TrimSpace(res.line)
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
		fmt.Println() // 回复结束后空一行，与下一轮 prompt 视觉分隔
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

	fmt.Print("\n\033[1;32m助手 >\033[0m \033[90m思考中\033[0m")

	// 启动"思考中..."闪动动画
	animDone := make(chan struct{})
	// animExited 在动画 goroutine 退出时关闭，用于确保调用方在写 stdout 前
	// 已等到 goroutine 完全退出，消除两端并发写 stdout 的竞态。
	animExited := make(chan struct{})
	// sync.Once 保证 animDone 只被 close 一次，避免重复 close 引发 panic。
	var closeAnim sync.Once
	closeAnimFn := func() { closeAnim.Do(func() { close(animDone) }) }
	// 函数退出时：先通知动画停止，再等待其完全退出，防止 goroutine 泄漏。
	defer func() { closeAnimFn(); <-animExited }()

	go func() {
		defer close(animExited) // goroutine 退出时通知等待方
		dots := []string{".", "..", "..."}
		i := 0
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-animDone:
				return
			case <-ticker.C:
				// \033[4D 回退4格（"..." + 一个空格的宽度），\033[K 清到行尾
				fmt.Printf("\033[4D\033[K\033[90m%-3s\033[0m", dots[i%len(dots)])
				i++
			}
		}
	}()

	firstToken := true
	if err := ag.Run(ctx, input, func(ev agent.Event) {
		if firstToken && ev.Type == agent.EventToken {
			closeAnimFn() // 通知动画退出
			<-animExited  // 等待动画 goroutine 完全退出，再写 stdout，消除竞态
			// 清除整行，重新打印"助手 >"
			fmt.Print("\r\033[K\033[1;32m助手 >\033[0m ")
			firstToken = false
		}
		streamToTerminal(ev)
	}); err != nil {
		if firstToken {
			closeAnimFn() // 出错时也要停止动画（defer 会等待退出）
		}
		if errors.Is(err, context.Canceled) {
			// 用户主动取消，不视为错误。
			return
		}
		fmt.Fprintf(os.Stderr, "\n\033[31m错误: %v\033[0m\n", err)
		return
	}
	// 确保动画已完全退出后再写 stdout（无 token 输出时 firstToken 仍为 true）。
	closeAnimFn()
	<-animExited
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
	fields := strings.Fields(line)
	// 纯 "/" 或仅含空白的输入会导致 fields 为空，直接返回避免越界 panic。
	if len(fields) == 0 {
		return false
	}
	cmd := fields[0]
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
