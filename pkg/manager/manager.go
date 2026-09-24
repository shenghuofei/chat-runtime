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
	"log/slog"
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
	// needCompress compress 模式下的待压缩标记。
	// appendAndMaybeOverflow 在锁内检测到超过 MaxRounds 时置位，
	// 真正的摘要（网络 IO）由 CompressIfNeeded 在锁外执行，
	// 避免在持写锁期间调用 Summarizer 阻塞其他并发操作。
	needCompress bool

	// normalizedCache 缓存最近一次 NormalizeToolMessages(m.messages) 的结果。
	// 仅在 normalizedDirty==false 时有效。
	normalizedCache []provider.Message
	// normalizedDirty 标记 m.messages 自上次标准化后是否被修改。
	// 初始为 true，每次变动 m.messages 时置 true；GetMessages 更新缓存后清零。
	normalizedDirty bool
	// roundCount 当前历史中的对话轮次数（以 user 消息为轮次起点）。
	// 随消息追加 / 裁剪 / 加载实时维护，避免在 markCompressIfNeededLocked 等处
	// 重复执行 countRoundsLocked() 的 O(N) 扫描。
	roundCount int
}

// New 创建一个 Manager。
func New(config ManagerConfig) *Manager {
	return &Manager{config: config, normalizedDirty: true}
}

// SetSystemPrompt 设置（或替换）系统提示词。
func (m *Manager) SetSystemPrompt(prompt string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.systemPrompt = prompt
}

// SystemPrompt 返回当前系统提示词（用于持久化快照）。
func (m *Manager) SystemPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.systemPrompt
}

// LastUsage 返回最近一次上报的 token 用量（用于持久化快照）。
func (m *Manager) LastUsage() provider.TokenUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastUsage
}

// LoadUsage 恢复持久化的 token 用量。
//
// 仅设置 lastUsage 字段，不触发 window 模式的裁剪逻辑。
// 与 ReportUsage 的区别：ReportUsage 是运行时上报（可能触发裁剪），
// LoadUsage 是会话恢复时还原已保存的状态（上下文已是裁剪后的结果，无需再裁）。
func (m *Manager) LoadUsage(usage provider.TokenUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUsage = usage
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
	m.normalizedDirty = true
	// 实时维护 roundCount，与 countRoundsLocked 的计数规则保持一致：
	//   - user 消息：新轮次开始，+1；
	//   - 首条非 user / 非 system 消息（边界情况）：也视为一轮起点，+1。
	switch {
	case msg.Role == provider.RoleUser:
		m.roundCount++
	case msg.Role != provider.RoleSystem && len(m.messages) == 1:
		m.roundCount++
	}

	switch m.config.Mode {
	case ModeTruncate:
		m.truncateByRoundsLocked()
	case ModeCompress:
		// compress 模式仅在锁内做「是否需要压缩」的判断并置标记，
		// 真正的摘要（网络 IO）由 CompressIfNeeded 在锁外执行，
		// 避免在持写锁期间调用 Summarizer 阻塞其他并发操作长达数十秒。
		m.markCompressIfNeededLocked()
	}
	// window 模式在 ReportUsage 时才裁剪（依赖真实 token 用量）。
}

// ReportUsage 上报最近一次请求的 token 用量。
//
// window 模式下，若用量超过阈值（MaxTokens*CompressRatio），则从最早轮次开始
// 逐轮删除，直到估算的剩余 token 数降到预算内或只剩 1 轮。
//
// 裁剪完全基于 estimateTokens 做决策（阈值判断和裁剪量用同一套度量），
// 不混用 Provider 上报的真实 token 数，避免两套口径不一致导致裁剪偏差。
// Provider 上报的 usage 仅用于记录/统计，不参与裁剪计算。
func (m *Manager) ReportUsage(usage provider.TokenUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUsage = usage

	if m.config.Mode != ModeWindow || m.config.MaxTokens <= 0 {
		return
	}

	// 完全基于估算值计算当前上下文总 token 数。
	totalEstimate := 0
	for _, msg := range m.messages {
		totalEstimate += estimateTokens(msg)
	}

	ratio := m.config.CompressRatio
	if ratio <= 0 {
		ratio = 1.0
	}
	threshold := int(float64(m.config.MaxTokens) * ratio)

	if totalEstimate <= threshold {
		return
	}

	// 超阈值：逐轮从最早开始删除，目标降到 MaxTokens 的 50%。
	target := m.config.MaxTokens / 2
	starts := m.roundStartIndices()
	if len(starts) <= 1 {
		return
	}

	remaining := totalEstimate
	dropUntilRound := 0
	for i := 0; i < len(starts)-1; i++ {
		if remaining <= target {
			break
		}
		end := starts[i+1]
		roundTokens := 0
		for j := starts[i]; j < end; j++ {
			roundTokens += estimateTokens(m.messages[j])
		}
		remaining -= roundTokens
		dropUntilRound = i + 1
	}

	keepRounds := len(starts) - dropUntilRound
	if keepRounds < 1 {
		keepRounds = 1
	}
	if keepRounds < len(starts) {
		m.keepLastRoundsLocked(keepRounds)
	}
}

// GetMessages 返回用于发送给模型的完整消息列表。
//
// 返回结果始终以 system 消息（若有）打头，其后为标准化后的历史消息。
//
// 优化：normalizedDirty 为 false 时直接复用缓存，避免重复执行 NormalizeToolMessages。
// 使用双重检查锁：先在 RLock 下判断，命中缓存时无需升级为写锁。
func (m *Manager) GetMessages() []provider.Message {
	// 快速路径：缓存有效，RLock 下直接使用。
	m.mu.RLock()
	if !m.normalizedDirty {
		body := m.normalizedCache
		sp := m.systemPrompt
		m.mu.RUnlock()
		out := make([]provider.Message, 0, len(body)+1)
		if sp != "" {
			out = append(out, provider.Message{Role: provider.RoleSystem, Content: sp})
		}
		out = append(out, body...)
		return out
	}
	m.mu.RUnlock()

	// 慢速路径：需要重新标准化，升级为写锁并更新缓存。
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.normalizedDirty {
		m.normalizedCache = NormalizeToolMessages(m.messages)
		m.normalizedDirty = false
	}
	out := make([]provider.Message, 0, len(m.normalizedCache)+1)
	if m.systemPrompt != "" {
		out = append(out, provider.Message{Role: provider.RoleSystem, Content: m.systemPrompt})
	}
	out = append(out, m.normalizedCache...)
	return out
}

// RoundCount 返回当前历史中的对话轮次数量（以 user 消息为轮次起点计数）。
func (m *Manager) RoundCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roundCount
}

// Load 用给定历史覆盖当前历史（用于会话恢复）。
func (m *Manager) Load(messages []provider.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = make([]provider.Message, len(messages))
	copy(m.messages, messages)
	m.normalizedDirty = true
	m.roundCount = m.countRoundsLocked()
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

// estimateTokens 基于消息内容长度估算 token 数。
//
// 粗略估算规则：1 token ≈ 4 个字符（英文），中文约 1 token ≈ 2 字符。
// 这里取折中值 3 字符/token，再加上消息结构开销（role、tool_call_id 等）。
// 虽然不如 tiktoken 精准，但足够用于裁剪决策。
func estimateTokens(msg provider.Message) int {
	// 消息结构固定开销（role、分隔符等），约 4 token。
	const overhead = 4
	charCount := len(msg.Content) + len(msg.Name)
	for _, tc := range msg.ToolCalls {
		charCount += len(tc.Name) + len(tc.Arguments)
	}
	return overhead + charCount/3
}

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
	m.messages = m.messages[start:]
	// 裁剪可能从某轮中间切开，导致 tool 结果失去对应的 assistant.tool_calls
	// （或反之）。标准化以修复游离的 tool 结果，保证配对合法。
	m.messages = NormalizeToolMessages(m.messages)
	// 规范化已在此处完成，直接更新缓存，GetMessages 无需重复计算。
	m.normalizedCache = m.messages
	m.normalizedDirty = false
	// keepLastRoundsLocked 精确保留 n 轮，同步字段避免后续重复扫描。
	m.roundCount = n
}

// truncateByRoundsLocked truncate 模式：超过 MaxRounds 时丢弃最早轮次。
//
// 直接委托给 keepLastRoundsLocked，无需先用 countRoundsLocked 做预检：
// keepLastRoundsLocked 内部已有 "len(starts) <= n" 的幂等守卫，未超限时直接返回，
// 省去一次 O(N) 的轮次计数。
func (m *Manager) truncateByRoundsLocked() {
	if m.config.MaxRounds <= 0 {
		return
	}
	m.keepLastRoundsLocked(m.config.MaxRounds)
}

// markCompressIfNeededLocked compress 模式：仅在锁内判断是否超过 MaxRounds，
// 若超过则置位 needCompress 标记，不在此处调用 Summarizer（避免持锁做网络 IO）。
// 实际摘要由 CompressIfNeeded 在锁外完成。
// 使用实时维护的 roundCount 字段，O(1) 判断，无需 O(N) 扫描。
func (m *Manager) markCompressIfNeededLocked() {
	if m.config.MaxRounds <= 0 || m.config.Summarizer == nil {
		return
	}
	if m.roundCount > m.config.MaxRounds {
		m.needCompress = true
	}
}

// CompressIfNeeded 在 compress 模式下按需执行早期历史的摘要压缩。
//
// 关键点：Summarizer 通常发起网络请求，可能阻塞数十秒。为避免在持写锁期间
// 做网络 IO 阻塞其他并发操作，本方法分三步：
//  1. 加锁提取待压缩的早期消息（early）与保留起点（keepFrom），随即解锁；
//  2. 锁外调用 Summarizer 生成摘要（耗时的网络 IO）；
//  3. 再次加锁，用一条 system 摘要消息替换早期历史。
//
// 由于第 2 步在锁外执行，期间历史可能被其他 goroutine 追加消息，因此第 3 步
// 基于「摘要覆盖的消息条数」而非绝对下标来做替换，保证并发安全。
//
// 该方法应在每轮 LLM 调用前由 Agent 调用。无待压缩标记时直接返回。
func (m *Manager) CompressIfNeeded() {
	// 步骤 1：锁内判断并提取待压缩消息。
	m.mu.Lock()
	if !m.needCompress || m.config.Summarizer == nil {
		m.needCompress = false
		m.mu.Unlock()
		return
	}
	// 使用实时维护的 roundCount 字段，O(1) 判断，无需 O(N) 扫描。
	if m.roundCount <= m.config.MaxRounds {
		// 已不再超限（可能已被其他裁剪处理），清除标记。
		m.needCompress = false
		m.mu.Unlock()
		return
	}
	starts := m.roundStartIndices()
	// 需要保留最近 MaxRounds 轮，其起点为 starts[len-MaxRounds]。
	keepFrom := starts[len(starts)-m.config.MaxRounds]
	if keepFrom <= 0 {
		m.needCompress = false
		m.mu.Unlock()
		return
	}
	// 拷贝待压缩消息，避免锁外访问共享切片。
	early := make([]provider.Message, keepFrom)
	copy(early, m.messages[:keepFrom])
	// 记录待压缩消息条数，作为锁外摘要完成后替换的依据。
	compressCount := keepFrom
	m.mu.Unlock()

	// 步骤 2：锁外执行摘要（可能是耗时的网络 IO）。
	summary, err := m.config.Summarizer(early)

	// 步骤 3：重新加锁，替换早期历史。
	m.mu.Lock()
	defer m.mu.Unlock()
	m.needCompress = false

	// 防御：锁外期间历史可能被 Load 覆盖或裁剪，若当前消息数已不足 compressCount，
	// 则放弃本次压缩，交由后续轮次重新判断。
	if compressCount > len(m.messages) {
		return
	}

	if err != nil {
		// 摘要失败则退化为直接截断，保证上下文体积可控。
		slog.Warn("compress 摘要失败，退化为截断早期历史", "error", err, "truncated_messages", compressCount)
		m.messages = m.messages[compressCount:]
		m.messages = NormalizeToolMessages(m.messages)
		m.normalizedCache = m.messages
		m.normalizedDirty = false
		m.roundCount = m.countRoundsLocked()
		return
	}

	summaryMsg := provider.Message{
		Role:    provider.RoleSystem,
		Content: "【早期对话摘要】\n" + summary,
	}
	rest := m.messages[compressCount:]
	newMsgs := make([]provider.Message, 0, len(rest)+1)
	newMsgs = append(newMsgs, summaryMsg)
	newMsgs = append(newMsgs, rest...)
	m.messages = newMsgs
	// 保留部分可能从某轮中间开始，标准化以修复游离的 tool 结果。
	m.messages = NormalizeToolMessages(m.messages)
	m.normalizedCache = m.messages
	m.normalizedDirty = false
	// 压缩后重新计算轮次。此路径已含 LLM 调用，O(N) 重算无额外影响。
	// 注：步骤 2（锁外）期间可能有新消息追加，直接数比用 MaxRounds 更准确。
	m.roundCount = m.countRoundsLocked()
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
