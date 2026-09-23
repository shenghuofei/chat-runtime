// Package manager 负责会话的上下文管理。
//
// 它维护单次会话的消息历史（不含 system，system 单独保存并固定在最前，
// 作为最稳定的 Prompt Cache 前缀），并在上下文超出预算时执行溢出策略
// （compress / truncate / window），保证发送给模型的请求不超过上下文窗口，
// 同时尽量保留关键信息。
//
// 该包还提供工具消息的标准化（NormalizeToolMessages）与轮次配对校验
// （ValidateRound），保证 assistant 的 tool_calls 与 tool 结果一一对应，
// 避免因乱序或缺失导致 Provider 报错。
package manager

import (
	"fmt"
	"sync"

	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// Mode 表示上下文溢出策略模式。
type Mode string

const (
	// ModeCompress 摘要压缩：将早期历史压缩为摘要消息（需额外 LLM 调用）。
	ModeCompress Mode = "compress"
	// ModeTruncate 截断：按轮次丢弃最早的消息。
	ModeTruncate Mode = "truncate"
	// ModeWindow 滑动窗口：按 token 用量维持窗口。
	ModeWindow Mode = "window"
)

// Summarizer 是 compress 模式下用于生成摘要的回调。
//
// 入参为待压缩的早期消息，返回一段摘要文本；返回错误时压缩会被跳过。
type Summarizer func(messages []provider.Message) (string, error)

// ManagerConfig 上下文管理配置。
//
// 带 yaml 标签，便于直接嵌入到全局配置（config.Config）中解析。
type ManagerConfig struct {
	// Mode 溢出策略模式。
	Mode Mode `yaml:"mode"`
	// MaxTokens window 模式下的 token 预算上限。
	MaxTokens int `yaml:"max_tokens"`
	// MaxRounds truncate / compress 模式下保留的最大对话轮次。
	MaxRounds int `yaml:"max_rounds"`
	// CompressRatio 触发比例。window 模式下，用量超过 MaxTokens*CompressRatio 即触发裁剪。
	CompressRatio float64 `yaml:"compress_ratio"`
	// Summarizer compress 模式下的摘要生成回调（不参与 YAML 解析）。
	Summarizer Summarizer `yaml:"-"`
}

// Manager 管理单个会话的消息历史与上下文预算。
//
// Manager 是并发安全的：Agent 在 Tool Calling 循环中可能从不同 goroutine
// 追加消息或读取历史。
type Manager struct {
	mu sync.RWMutex

	// systemPrompt 系统提示词文本，始终固定在历史最前。
	systemPrompt string
	// messages 除 system 外的消息历史（按时间顺序追加）。
	messages []provider.Message
	// lastUsage 最近一次上报的 token 用量（window 模式使用）。
	lastUsage provider.TokenUsage
	// config 上下文管理配置。
	config ManagerConfig
}

// New 创建一个 Manager。
func New(config ManagerConfig) *Manager {
	return &Manager{config: config}
}

// SetSystemPrompt 设置（或替换）系统提示词。
func (m *Manager) SetSystemPrompt(prompt string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.systemPrompt = prompt
}

// AddUserMessage 追加一条用户消息，并在追加后按策略检查是否需要裁剪。
func (m *Manager) AddUserMessage(msg provider.Message) {
	msg.Role = provider.RoleUser
	m.appendAndMaybeOverflow(msg)
}

// AddAssistantMessage 追加一条助手消息（可能携带 tool_calls）。
func (m *Manager) AddAssistantMessage(msg provider.Message) {
	msg.Role = provider.RoleAssistant
	m.appendAndMaybeOverflow(msg)
}

// AddToolResult 追加一条工具结果消息，归属于当前轮次。
func (m *Manager) AddToolResult(toolCallID, name, content string) {
	m.appendAndMaybeOverflow(provider.Message{
		Role:       provider.RoleTool,
		ToolCallID: toolCallID,
		Name:       name,
		Content:    content,
	})
}

// appendAndMaybeOverflow 追加消息，并按当前模式触发裁剪。
func (m *Manager) appendAndMaybeOverflow(msg provider.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)

	switch m.config.Mode {
	case ModeTruncate:
		m.truncateByRoundsLocked()
	case ModeCompress:
		m.compressByRoundsLocked()
	}
	// window 模式在 ReportUsage 时才裁剪（依赖真实 token 用量）。
}

// ReportUsage 上报最近一次请求的 token 用量。
//
// window 模式下，若用量超过阈值（MaxTokens*CompressRatio），则从最早轮次开始
// 丢弃，直到轮次数量减半（至少保留一轮），以快速把上下文降回预算内。
func (m *Manager) ReportUsage(usage provider.TokenUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUsage = usage

	if m.config.Mode != ModeWindow || m.config.MaxTokens <= 0 {
		return
	}

	ratio := m.config.CompressRatio
	if ratio <= 0 {
		ratio = 1.0
	}
	threshold := int(float64(m.config.MaxTokens) * ratio)

	if usage.TotalTokens <= threshold {
		return
	}

	// 超阈值：将轮次数量减半（向上取整，至少保留 1 轮）。
	rounds := m.countRoundsLocked()
	if rounds <= 1 {
		return
	}
	target := (rounds + 1) / 2
	m.keepLastRoundsLocked(target)
}

// GetMessages 返回用于发送给模型的完整消息列表。
//
// 返回结果始终以 system 消息（若有）打头，其后为标准化后的历史消息。
func (m *Manager) GetMessages() []provider.Message {
	m.mu.RLock()
	defer m.mu.RUnlock()

	body := NormalizeToolMessages(m.messages)

	out := make([]provider.Message, 0, len(body)+1)
	if m.systemPrompt != "" {
		out = append(out, provider.Message{Role: provider.RoleSystem, Content: m.systemPrompt})
	}
	out = append(out, body...)
	return out
}

// RoundCount 返回当前历史中的对话轮次数量（以 user 消息为轮次起点计数）。
func (m *Manager) RoundCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.countRoundsLocked()
}

// Load 用给定历史覆盖当前历史（用于会话恢复）。
func (m *Manager) Load(messages []provider.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = make([]provider.Message, len(messages))
	copy(m.messages, messages)
}

// Raw 返回不含 system、未经标准化的原始历史副本（用于持久化）。
func (m *Manager) Raw() []provider.Message {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make([]provider.Message, len(m.messages))
	copy(cp, m.messages)
	return cp
}

// -------------------- 内部裁剪逻辑（调用方需持有锁） --------------------

// countRoundsLocked 统计历史中的轮次数量。
//
// 一“轮”以一条 user 消息为起点。compress 模式会在历史开头插入一条 system
// 摘要消息，这类前导 system 消息不计入轮次。若历史开头是非 user、非 system
// 的消息（例如以 assistant 起始的边界情况），则将其视为一轮，避免计数为 0
// 而无法裁剪。
func (m *Manager) countRoundsLocked() int {
	rounds := 0
	for i, msg := range m.messages {
		switch {
		case msg.Role == provider.RoleUser:
			rounds++
		case msg.Role == provider.RoleSystem:
			// 前导摘要等 system 消息不计入轮次。
			continue
		case i == 0:
			// 首条既非 user 也非 system，计作一轮的开始。
			rounds++
		}
	}
	return rounds
}

// roundStartIndices 返回每一轮起点在 messages 中的下标。
func (m *Manager) roundStartIndices() []int {
	var starts []int
	for i, msg := range m.messages {
		switch {
		case msg.Role == provider.RoleUser:
			starts = append(starts, i)
		case msg.Role == provider.RoleSystem:
			continue
		case i == 0:
			starts = append(starts, 0)
		}
	}
	return starts
}

// keepLastRoundsLocked 仅保留最近 n 轮消息。
func (m *Manager) keepLastRoundsLocked(n int) {
	if n <= 0 {
		return
	}
	starts := m.roundStartIndices()
	if len(starts) <= n {
		return
	}
	start := starts[len(starts)-n]
	m.messages = append([]provider.Message(nil), m.messages[start:]...)
}

// truncateByRoundsLocked truncate 模式：超过 MaxRounds 时丢弃最早轮次。
func (m *Manager) truncateByRoundsLocked() {
	if m.config.MaxRounds <= 0 {
		return
	}
	if m.countRoundsLocked() > m.config.MaxRounds {
		m.keepLastRoundsLocked(m.config.MaxRounds)
	}
}

// compressByRoundsLocked compress 模式：超过 MaxRounds 时，把超出的早期轮次
// 交给 Summarizer 生成摘要，用一条 system 摘要消息替换它们。
func (m *Manager) compressByRoundsLocked() {
	if m.config.MaxRounds <= 0 || m.config.Summarizer == nil {
		return
	}
	rounds := m.countRoundsLocked()
	if rounds <= m.config.MaxRounds {
		return
	}

	starts := m.roundStartIndices()
	// 需要保留最近 MaxRounds 轮，其起点为 starts[len-MaxRounds]。
	keepFrom := starts[len(starts)-m.config.MaxRounds]

	early := m.messages[:keepFrom]
	if len(early) == 0 {
		return
	}

	summary, err := m.config.Summarizer(early)
	if err != nil {
		// 摘要失败则退化为直接截断，保证上下文体积可控。
		m.messages = append([]provider.Message(nil), m.messages[keepFrom:]...)
		return
	}

	summaryMsg := provider.Message{
		Role:    provider.RoleSystem,
		Content: "【早期对话摘要】\n" + summary,
	}
	rest := m.messages[keepFrom:]
	newMsgs := make([]provider.Message, 0, len(rest)+1)
	newMsgs = append(newMsgs, summaryMsg)
	newMsgs = append(newMsgs, rest...)
	m.messages = newMsgs
}

// -------------------- 工具消息标准化与校验（无状态工具函数） --------------------

// NormalizeToolMessages 对消息序列做标准化，确保每个 assistant 的 tool_calls
// 之后紧跟着按相同顺序排列的 tool 结果。
//
// 处理规则：
//   - 对每条含 tool_calls 的 assistant 消息，按其 tool_calls 顺序收集对应的
//     tool 结果，重排后紧随该 assistant 消息输出；
//   - 未能匹配到任何 assistant 的“孤立” tool 结果予以保留（追加在末尾），
//     避免因数据异常而丢弃信息。
func NormalizeToolMessages(messages []provider.Message) []provider.Message {
	// 建立 tool_call_id -> tool 结果消息 的索引，同时记录被消费状态。
	toolByID := make(map[string]provider.Message)
	consumed := make(map[string]bool)
	for _, msg := range messages {
		if msg.Role == provider.RoleTool && msg.ToolCallID != "" {
			toolByID[msg.ToolCallID] = msg
		}
	}

	out := make([]provider.Message, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case provider.RoleTool:
			// tool 结果不在此处直接输出，交由对应 assistant 处理后统一排布。
			continue
		case provider.RoleAssistant:
			out = append(out, msg)
			// 按 tool_calls 顺序回填对应结果。
			for _, tc := range msg.ToolCalls {
				if res, ok := toolByID[tc.ID]; ok {
					out = append(out, res)
					consumed[tc.ID] = true
				}
			}
		default:
			out = append(out, msg)
		}
	}

	// 追加未被任何 assistant 消费的孤立 tool 结果，保持原始相对顺序。
	for _, msg := range messages {
		if msg.Role == provider.RoleTool && msg.ToolCallID != "" && !consumed[msg.ToolCallID] {
			out = append(out, msg)
		}
	}

	return out
}

// ValidateRound 校验消息序列中 assistant 的 tool_calls 与 tool 结果是否配对合法。
//
// 校验项：
//   - 每个 assistant.tool_calls 中的 ID 都必须有对应的 tool 结果（缺失则报错）；
//   - 每个 tool 结果的 ToolCallID 都必须能在某个 assistant.tool_calls 中找到
//     （游离结果则报错）。
func ValidateRound(messages []provider.Message) error {
	declared := make(map[string]bool) // assistant 声明的 tool_call ID
	answered := make(map[string]bool) // tool 结果覆盖的 ID

	for _, msg := range messages {
		if msg.Role == provider.RoleAssistant {
			for _, tc := range msg.ToolCalls {
				declared[tc.ID] = true
			}
		}
	}
	for _, msg := range messages {
		if msg.Role == provider.RoleTool {
			if msg.ToolCallID == "" || !declared[msg.ToolCallID] {
				return fmt.Errorf("游离的工具结果：tool_call_id=%q 未匹配任何 tool_calls", msg.ToolCallID)
			}
			answered[msg.ToolCallID] = true
		}
	}
	for id := range declared {
		if !answered[id] {
			return fmt.Errorf("缺少工具结果：tool_call_id=%q 未回填", id)
		}
	}
	return nil
}
