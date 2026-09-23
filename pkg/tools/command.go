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
	"regexp"
	"strings"
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
}

// defaultCommandTimeout 默认命令执行超时。
const defaultCommandTimeout = 30 * time.Second

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
	regexp.MustCompile(`>>?`), // > 或 >> 重定向（覆盖/追加）
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
	config            CommandToolConfig
	dangerousPatterns []*regexp.Regexp
}

// NewCommandTool 创建一个命令执行工具。
//
// 会将内置 DangerousPatterns 与配置中的 BlockedPatterns 合并作为最终的危险模式集合。
func NewCommandTool(cfg CommandToolConfig) *CommandTool {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultCommandTimeout
	}
	patterns := make([]*regexp.Regexp, 0, len(DangerousPatterns)+len(cfg.BlockedPatterns))
	patterns = append(patterns, DangerousPatterns...)
	patterns = append(patterns, cfg.BlockedPatterns...)
	return &CommandTool{config: cfg, dangerousPatterns: patterns}
}

// Name 返回工具名称。
func (t *CommandTool) Name() string { return "command" }

// Description 返回工具描述。
func (t *CommandTool) Description() string {
	return "在受控环境中执行 shell 命令。参数以 command（可执行文件名）与 args（参数数组）给出，" +
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

// NeedApproval 返回该工具是否需要审批。
//
// 命令工具作为高危能力，默认统一要求审批（返回 true）。更精细的“仅危险命令
// 需审批”判定，请使用 IsDangerous 在调用点动态决策。
func (t *CommandTool) NeedApproval() bool { return true }

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
func fullCommandLine(args commandArgs) string {
	parts := append([]string{args.Command}, args.Args...)
	return strings.Join(parts, " ")
}

// baseName 提取命令的基础名（去除路径），用于白名单匹配。
func baseName(command string) string {
	if idx := strings.LastIndexAny(command, "/\\"); idx >= 0 {
		return command[idx+1:]
	}
	return command
}

// IsAllowed 判断命令是否在白名单内。白名单为空时视为放行。
func (t *CommandTool) IsAllowed(command string) bool {
	if len(t.config.AllowedCommands) == 0 {
		return true
	}
	name := baseName(command)
	for _, allowed := range t.config.AllowedCommands {
		if name == allowed || command == allowed {
			return true
		}
	}
	return false
}

// IsDangerous 判断完整命令行是否命中任一危险模式。
func (t *CommandTool) IsDangerous(commandLine string) bool {
	for _, re := range t.dangerousPatterns {
		if re.MatchString(commandLine) {
			return true
		}
	}
	return false
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

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()

	// 超时判定优先。
	if execCtx.Err() == context.DeadlineExceeded {
		return buf.String(), fmt.Errorf("命令执行超时（%s）", t.config.Timeout)
	}
	if runErr != nil {
		// 非零退出：把已捕获的输出一并返回，便于模型理解失败原因。
		return buf.String(), fmt.Errorf("命令执行失败：%w", runErr)
	}
	return buf.String(), nil
}
