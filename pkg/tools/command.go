// Package tools 提供内置工具实现。
//
// 本文件实现命令执行工具（CommandTool）：这是风险最高的能力，因此内置了
// 多重安全约束——命令白名单、危险模式（正则）黑名单、以及“命中危险模式即
// 需要审批”的策略。执行时使用 exec.Command 的参数数组形式（绝不使用
// `sh -c`），从根本上规避 shell 注入。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// CommandToolConfig 是命令执行工具的配置。
type CommandToolConfig struct {
	// AllowedCommands 命令白名单（按可执行文件名，如 "ls"、"kubectl"）。
	// 为空表示不启用白名单（放行任意命令，但仍受危险模式约束）。
	AllowedCommands []string
	// BlockedPatterns 额外的自定义危险模式（会与内置 DangerousPatterns 合并）。
	BlockedPatterns []*regexp.Regexp
	// Timeout 单条命令执行超时（<=0 时取默认值 30s）。
	Timeout time.Duration
	// WorkDir 命令执行的工作目录（为空则继承当前进程工作目录）。
	WorkDir string
	// AutoApprove 为 true 时所有命令自动批准（跳过审批），无论是否命中危险模式。
	// 默认 false，即危险命令需人工审批。
	AutoApprove bool
	// MaxOutputBytes 单次命令输出的字节上限。
	// 超出后采用"头尾各保留 50%"策略，中间用省略标记替代。
	// <=0 时使用默认值 1 MiB；-1 表示不限制（谨慎使用）。
	MaxOutputBytes int
}

// defaultCommandTimeout 默认命令执行超时。
const defaultCommandTimeout = 30 * time.Second

// defaultMaxOutputBytes 命令输出的默认字节上限（1 MiB）。
const defaultMaxOutputBytes = 1 * 1024 * 1024

// DangerousPatterns 是内置的危险命令正则模式集合（约 40 条）。
//
// 命中其中任意一条即被判定为“危险命令”：此时 NeedApproval() 返回 true，
// 由上层审批流决定是否放行；未命中的命令视为相对安全。
//
// 说明：这些模式作用于“完整命令行字符串”（可执行名 + 参数拼接），
// 用于识别常见的破坏性/高危操作。
var DangerousPatterns = []*regexp.Regexp{
	// —— 删除类 ——
	regexp.MustCompile(`\brm\b.*\s-[a-zA-Z]*r[a-zA-Z]*f`), // rm -rf / rm -fr 等
	regexp.MustCompile(`\brm\b.*\s-[a-zA-Z]*f[a-zA-Z]*r`),
	regexp.MustCompile(`\brm\s+-[a-zA-Z]*r`), // 递归删除
	regexp.MustCompile(`\brm\s+.*/\s*$`),     // 删除以 / 结尾的路径
	regexp.MustCompile(`\brmdir\b`),
	regexp.MustCompile(`\bunlink\b`),
	regexp.MustCompile(`\bshred\b`),

	// —— 磁盘/文件系统类 ——
	regexp.MustCompile(`\bdd\b`),           // dd
	regexp.MustCompile(`\bmkfs(\.\w+)?\b`), // mkfs / mkfs.ext4 等
	regexp.MustCompile(`\bfdisk\b`),
	regexp.MustCompile(`\bparted\b`),
	regexp.MustCompile(`\bwipefs\b`),
	regexp.MustCompile(`\bblkdiscard\b`),
	regexp.MustCompile(`\bformat\b`),
	regexp.MustCompile(`>\s*/dev/sd[a-z]`), // 写入块设备

	// —— 挂载类 ——
	regexp.MustCompile(`\bmount\b`),
	regexp.MustCompile(`\bumount\b`),

	// —— 重定向/覆盖类 ——
	// 匹配空白后跟 > 或 >> 的 shell 重定向操作符（如 `cmd > file` 或 `cmd >> file`）。
	// 要求运算符前有空白，避免误匹配字符串内嵌的 > 符号（如 URL、比较表达式）。
	regexp.MustCompile(`\s>>?`), // > 或 >> 重定向（覆盖/追加）
	regexp.MustCompile(`\btee\b`),
	regexp.MustCompile(`\btruncate\b`),

	// —— 权限/属主类 ——
	regexp.MustCompile(`\bchmod\b.*\s(-R|777|0?777)`), // 递归/全开权限
	regexp.MustCompile(`\bchown\b.*\s-R`),
	regexp.MustCompile(`\bchattr\b`),

	// —— 提权类 ——
	regexp.MustCompile(`\bsudo\b`),
	regexp.MustCompile(`\bsu\b`),
	regexp.MustCompile(`\bdoas\b`),

	// —— 进程/系统控制类 ——
	regexp.MustCompile(`\bkill(all)?\b`),
	regexp.MustCompile(`\bpkill\b`),
	regexp.MustCompile(`\bshutdown\b`),
	regexp.MustCompile(`\breboot\b`),
	regexp.MustCompile(`\bhalt\b`),
	regexp.MustCompile(`\bpoweroff\b`),
	regexp.MustCompile(`\binit\s+[06]\b`),
	regexp.MustCompile(`\bsystemctl\b.*\b(stop|disable|mask)\b`),

	// —— 危险的管道执行（远程脚本直接执行）——
	regexp.MustCompile(`\bcurl\b.*\|\s*(sh|bash|zsh)`),
	regexp.MustCompile(`\bwget\b.*\|\s*(sh|bash|zsh)`),
	regexp.MustCompile(`\beval\b`),
	regexp.MustCompile(`\bexec\b`),

	// —— 包管理/全局变更类 ——
	regexp.MustCompile(`\bapt(-get)?\b.*\bremove\b`),
	regexp.MustCompile(`\byum\b.*\b(remove|erase)\b`),
	regexp.MustCompile(`\bnpm\b.*\b(uninstall|-g)\b`),

	// —— 版本控制破坏类 ——
	regexp.MustCompile(`\bgit\b.*\breset\b.*--hard`),
	regexp.MustCompile(`\bgit\b.*\bclean\b.*-[a-zA-Z]*f`),
	regexp.MustCompile(`\bgit\b.*push.*--force`),

	// —— Fork 炸弹 ——
	regexp.MustCompile(`:\(\)\s*\{.*\};:`),

	// —— 危险路径 ——
	regexp.MustCompile(`\s/\s*$`), // 以根路径结尾
	regexp.MustCompile(`\s~\s*$`), // 以家目录结尾
	regexp.MustCompile(`/etc/(passwd|shadow|sudoers)`),
}

// commandArgs 是命令工具的入参结构（模型给出的 JSON）。
type commandArgs struct {
	// Command 可执行文件名或路径（如 "ls"、"/usr/bin/kubectl"）。
	Command string `json:"command"`
	// Args 命令参数数组。
	Args []string `json:"args,omitempty"`
}

// CommandTool 是安全的命令执行工具，实现 agent.Tool 接口。
type CommandTool struct {
	config CommandToolConfig
	// dangerousPatterns 保留原始列表，供外部（如测试）读取各模式。
	dangerousPatterns []*regexp.Regexp
	// combinedPattern 将所有危险模式合并为单个正则，匹配时只需一次调用。
	combinedPattern *regexp.Regexp
	// allowedSet 是 AllowedCommands 的 O(1) 查找索引（key 为命令名或完整路径）。
	allowedSet map[string]struct{}
}

// NewCommandTool 创建一个命令执行工具。
//
// 会将内置 DangerousPatterns 与配置中的 BlockedPatterns 合并作为最终的危险模式集合，
// 并预编译为单个合并正则（(?:pat1)|(?:pat2)|...），使 IsDangerous 只需一次匹配。
func NewCommandTool(cfg CommandToolConfig) *CommandTool {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultCommandTimeout
	}
	patterns := make([]*regexp.Regexp, 0, len(DangerousPatterns)+len(cfg.BlockedPatterns))
	patterns = append(patterns, DangerousPatterns...)
	patterns = append(patterns, cfg.BlockedPatterns...)

	// 构建合并正则：将各模式用 (?:...) 包裹后以 | 连接。
	parts := make([]string, len(patterns))
	for i, re := range patterns {
		parts[i] = "(?:" + re.String() + ")"
	}
	combined := regexp.MustCompile(strings.Join(parts, "|"))

	allowedSet := make(map[string]struct{}, len(cfg.AllowedCommands))
	for _, cmd := range cfg.AllowedCommands {
		allowedSet[cmd] = struct{}{}
	}

	return &CommandTool{
		config:            cfg,
		dangerousPatterns: patterns,
		combinedPattern:   combined,
		allowedSet:        allowedSet,
	}
}

// Name 返回工具名称。
func (t *CommandTool) Name() string { return "command" }

// Description 返回工具描述。
func (t *CommandTool) Description() string {
	return "在受控环境中执行系统命令。参数以 command（可执行文件名）与 args（参数数组）给出，" +
		"不支持管道、重定向等 shell 语法。危险命令将触发人工审批。"
}

// Schema 返回工具的 JSON Schema 定义。
func (t *CommandTool) Schema() provider.ToolDef {
	return provider.ToolDef{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "要执行的可执行文件名或绝对路径，例如 ls、kubectl。",
				},
				"args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "命令参数数组，例如 [\"-l\", \"/tmp\"]。",
				},
			},
			"required": []string{"command"},
		},
	}
}

// NeedApproval 返回该工具是否可能需要审批。
//
// CommandTool 标记为”可能需要审批”（true），使其进入 approvalTool 装饰流程。
// 实际上只有危险命令才会触发审批弹窗——approvalTool 会调用 NeedApprovalFor
// 按参数动态判断，安全命令（ls、pwd 等）不会打扰用户。
func (t *CommandTool) NeedApproval() bool { return !t.config.AutoApprove }

// NeedApprovalFor 根据实际调用参数判断是否需要审批。
//
// 实现 agent.ArgumentAwareApproval 接口：只有 IsDangerous 判定为危险的命令
// 才触发审批请求，普通只读命令直接放行。参数解析失败时保守地返回 true。
func (t *CommandTool) NeedApprovalFor(arguments string) bool {
	args, err := t.parseArgs(arguments)
	if err != nil {
		return true // 无法解析时保守处理，要求审批
	}
	return t.IsDangerous(fullCommandLine(args))
}

// parseArgs 解析并基础校验模型给出的参数。
func (t *CommandTool) parseArgs(arguments string) (commandArgs, error) {
	var args commandArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return args, fmt.Errorf("解析命令参数失败：%w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return args, fmt.Errorf("command 不能为空")
	}
	return args, nil
}

// fullCommandLine 将 command 与 args 拼接为完整命令行字符串（仅用于模式匹配与展示）。
//
// 拼接后将换行符（\n \r）规范化为空格，防止参数中嵌入换行符绕过单行危险模式正则。
// 例如 args=["foo\nrm -rf /"] 若不规范化，"rm -rf" 出现在第二行会跳过所有无多行标志的正则。
func fullCommandLine(args commandArgs) string {
	var b strings.Builder
	b.WriteString(args.Command)
	for _, a := range args.Args {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	s := b.String()
	// 规范化换行符，避免多行参数绕过单行正则匹配。
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// baseName 提取命令的基础名（去除路径），用于白名单匹配。
// 使用 filepath.Base 以正确处理各平台路径分隔符。
func baseName(command string) string {
	return filepath.Base(command)
}

// IsAllowed 判断命令是否在白名单内。白名单为空时视为放行。
//
// 使用预建的 allowedSet map 做 O(1) 查找，避免每次执行命令时的 O(N) 线性遍历。
func (t *CommandTool) IsAllowed(command string) bool {
	if len(t.allowedSet) == 0 {
		return true
	}
	name := baseName(command)
	_, byName := t.allowedSet[name]
	_, byFull := t.allowedSet[command]
	return byName || byFull
}

// IsDangerous 判断完整命令行是否命中任一危险模式。
//
// 使用预编译的合并正则进行单次匹配，性能优于逐条遍历。
func (t *CommandTool) IsDangerous(commandLine string) bool {
	return t.combinedPattern.MatchString(commandLine)
}

// Execute 执行命令。
//
// 安全流程：
//  1. 解析并校验参数；
//  2. 白名单校验：未命中则直接拒绝；
//  3. 使用 exec.CommandContext 以参数数组执行（不经过 shell）；
//  4. 受 ctx 与配置超时约束；
//  5. 合并返回 stdout + stderr。
//
// 注意：危险命令的“审批”由上层通过 WrapWithApproval + NeedApproval 处理；
// Execute 本身聚焦于白名单与安全执行。
func (t *CommandTool) Execute(ctx context.Context, arguments string) (string, error) {
	args, err := t.parseArgs(arguments)
	if err != nil {
		return "", err
	}

	// 白名单校验。
	if !t.IsAllowed(args.Command) {
		return "", fmt.Errorf("命令 %q 不在白名单内，已拒绝执行", args.Command)
	}

	// 施加超时（取 ctx 与配置超时的较小者：这里以配置超时再包一层）。
	execCtx := ctx
	if t.config.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, t.config.Timeout)
		defer cancel()
	}

	// 关键安全点：使用参数数组形式，绝不使用 `sh -c`，避免 shell 注入。
	cmd := exec.CommandContext(execCtx, args.Command, args.Args...)
	if t.config.WorkDir != "" {
		cmd.Dir = t.config.WorkDir
	}

	// 确定输出字节上限：0 使用默认值，负数表示不限制。
	limit := t.config.MaxOutputBytes
	if limit == 0 {
		limit = defaultMaxOutputBytes
	}
	// 采集容量：正常情况用 2× limit 以支持头尾提取；
	// 不限制模式（limit<0）使用 64 MiB 安全兜底，防止 OOM。
	capBytes := 64 * 1024 * 1024
	if limit > 0 {
		capBytes = limit * 2
	}
	lw := &limitedWriter{limit: capBytes}
	cmd.Stdout = lw
	cmd.Stderr = lw

	runErr := cmd.Run()

	raw := lw.buf.Bytes()
	var output string
	if limit > 0 && len(raw) > limit {
		// 超出上限：保留头尾各 50%，中间用省略标记替代。
		half := limit / 2
		head := raw[:half]
		tail := raw[len(raw)-half:]
		omitted := int64(len(raw)) - int64(half)*2
		var marker string
		if lw.truncated {
			// 实际输出超过采集上限（2×limit），tail 仅为近似尾部。
			marker = fmt.Sprintf("\n[...中间内容已省略（≥%d 字节，实际可能更多）...]\n", omitted)
		} else {
			marker = fmt.Sprintf("\n[...中间内容已省略约 %d 字节...]\n", omitted)
		}
		output = string(head) + marker + string(tail)
	} else {
		output = string(raw)
		if lw.truncated {
			// 命中了不限制模式的 64 MiB 安全上限。
			output += fmt.Sprintf("\n[输出已截断，超过 %d 字节安全上限]", capBytes)
		}
	}

	// 超时判定优先。
	if execCtx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("命令执行超时（%s）", t.config.Timeout)
	}
	if runErr != nil {
		// 非零退出：把已捕获的输出一并返回，便于模型理解失败原因。
		return output, fmt.Errorf("命令执行失败：%w", runErr)
	}
	return output, nil
}

// limitedWriter 是一个包装 bytes.Buffer 的写入器，超过 limit 字节后停止写入
// 并标记 truncated，防止命令产生海量输出耗尽内存。
//
// exec.Command 会将 Stdout 与 Stderr 指向同一个 limitedWriter，
// 且由两个独立的 goroutine 并发写入，因此需要 mutex 保护内部状态。
type limitedWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.truncated {
		return len(p), nil // 已截断，丢弃后续数据
	}
	remaining := lw.limit - lw.buf.Len()
	if remaining <= 0 {
		lw.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		lw.truncated = true
		p = p[:remaining]
	}
	return lw.buf.Write(p)
}
