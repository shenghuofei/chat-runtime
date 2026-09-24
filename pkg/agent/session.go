package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/manager"
	"github.com/shenghuofei/chat-runtime/pkg/provider"
	"github.com/shenghuofei/chat-runtime/pkg/store"
)

// ProviderFactory 创建（或复用）一个 Provider 实例。
//
// 由上层注入，屏蔽具体 Provider 的构造过程，
// 使 Session 不直接依赖配置解析逻辑。
type ProviderFactory func() (provider.Provider, error)
// Session 表示一次会话，聚合了 Agent、上下文管理器、持久化 Store 与元信息。
type Session struct {
	// ID 会话唯一标识。
	ID string
	// ChatName 会话所属的对话配置名。
	ChatName string
	// Agent 该会话使用的 Agent（编排 LLM + 工具循环）。
	Agent *Agent
	// Store 会话持久化后端。
	Store store.Store
	// CreatedAt 会话创建时间。
	CreatedAt time.Time
	// LastActiveAt 最近活跃时间（用于 LRU 淘汰）。
	LastActiveAt time.Time

	// provider 本会话持有的 Provider（关闭时释放）。
	provider provider.Provider
	// manager 本会话的上下文管理器。
	manager *manager.Manager
}

// NewSession 创建并初始化一个会话。
//
// 步骤：
//  1. 通过 providerFactory 根据 ChatConfig 创建 Provider；
//  2. 依据 ChatConfig 的上下文策略创建 Manager，并设置 system 提示词；
//  3. 若 Store 中存在历史快照，则加载以恢复消息历史；
//  4. 组装 Agent（工具由上层在返回后通过 RegisterTool 注入）。
func NewSession(
	id, chatName string,
	chatConfig config.ChatConfig,
	providerFactory ProviderFactory,
	st store.Store,
) (*Session, error) {
	if providerFactory == nil {
		return nil, fmt.Errorf("providerFactory 不能为空")
	}

	// 1. 创建 Provider。
	p, err := providerFactory()
	if err != nil {
		return nil, fmt.Errorf("创建 provider 失败：%w", err)
	}

	// 2. 创建上下文管理器：优先使用对话级策略，否则使用零值（读取时不裁剪）。
	var mgrCfg manager.ManagerConfig
	if chatConfig.ContextManager != nil {
		mgrCfg = *chatConfig.ContextManager
	}
	// compress 模式：注入 Summarizer，将早期消息通过 LLM 压缩为摘要。
	if mgrCfg.Mode == manager.ModeCompress && mgrCfg.Summarizer == nil {
		mgrCfg.Summarizer = buildSummarizer(p)
	}
	mgr := manager.New(mgrCfg)
	mgr.SetSystemPrompt(chatConfig.System)

	// 3. 尝试从 Store 恢复历史（合并 checkpoint 快照 + 增量日志）。
	//
	// 正常关闭流程：checkpoint 包含完整历史，log 为空（Save 后截断）；
	// 崩溃场景：checkpoint 是最后一次 Save 的快照，log 包含此后的增量消息，
	// 两者合并可还原完整历史。
	if st != nil {
		var msgs []provider.Message
		if data, err := st.Load(context.Background(), id); err == nil && data != nil {
			msgs = data.Messages
			if data.SystemPrompt != "" {
				mgr.SetSystemPrompt(data.SystemPrompt)
			}
			if data.TokenUsage.TotalTokens > 0 {
				// 仅设字段，不触发裁剪——上下文已是上次持久化时裁剪后的结果。
				mgr.LoadUsage(data.TokenUsage)
			}
		}
		if logMsgs, err := st.LoadLog(context.Background(), id); err == nil && len(logMsgs) > 0 {
			// Save→truncate 两步非原子：崩溃时可能出现 checkpoint 已含某些消息、
			// log 未被截断的窗口，导致 checkpoint 尾部与 log 头部重叠。
			// 合并前先去重，避免恢复出重复消息。
			logMsgs = deduplicateLog(msgs, logMsgs)
			msgs = append(msgs, logMsgs...)
		}
		if len(msgs) > 0 {
			if verr := manager.ValidateRound(msgs); verr != nil {
				slog.Warn("会话历史消息不完整，已尽力恢复", "session", id, "error", verr)
			}
			mgr.Load(msgs)
		}
	}

	// 4. 组装 Agent，注入增量持久化回调。
	agCfg := AgentConfig{MaxIterations: chatConfig.MaxIterations}
	if st != nil {
		agCfg.OnMessageAdded = func(msg provider.Message) {
			if err := st.AppendMessage(context.Background(), id, msg); err != nil {
				slog.Warn("增量持久化失败", "session", id, "role", msg.Role, "error", err)
			}
		}
	}
	ag := New(p, mgr, agCfg)

	now := time.Now()
	return &Session{
		ID:           id,
		ChatName:     chatName,
		Agent:        ag,
		Store:        st,
		CreatedAt:    now,
		LastActiveAt: now,
		provider:     p,
		manager:      mgr,
	}, nil
}

// deduplicateLog 去除增量日志中与 checkpoint 尾部重叠的前缀。
//
// 背景：Store.Save 采用「先 rename 提交快照、再 truncate 日志」两步，二者非原子。
// 若在两步之间进程崩溃，恢复时会得到「已包含尾部消息的 checkpoint」+「尚未截断、
// 头部与 checkpoint 尾部重叠的 log」，直接 append 会产生重复消息。
//
// 策略：从最大可能重叠长度开始递减，寻找 checkpoint 尾部与 log 头部完全相等的
// 前缀，命中则跳过 log 中重叠的部分，仅返回其后的新增消息。
func deduplicateLog(checkpoint, log []provider.Message) []provider.Message {
	if len(checkpoint) == 0 || len(log) == 0 {
		return log
	}
	// 崩溃恢复场景下重叠通常极少（1~2 条），从小到大扫描比从大到小更早命中，
	// 避免从最大可能值开始逐步缩小造成的 O(N²) 最坏情况。
	for overlap := 1; overlap <= min(len(checkpoint), len(log)); overlap++ {
		if messagesEqual(checkpoint[len(checkpoint)-overlap:], log[:overlap]) {
			return log[overlap:]
		}
	}
	return log
}

// messagesEqual 比较两个消息切片是否逐条相等（基于角色、内容、ToolCallID、
// 工具名与工具调用列表）。用于 deduplicateLog 判定前缀重叠。
func messagesEqual(a, b []provider.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !messageEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// messageEqual 比较单条消息是否相等。
func messageEqual(x, y provider.Message) bool {
	if x.Role != y.Role || x.Content != y.Content ||
		x.ToolCallID != y.ToolCallID || x.Name != y.Name {
		return false
	}
	if len(x.ToolCalls) != len(y.ToolCalls) {
		return false
	}
	for i := range x.ToolCalls {
		if x.ToolCalls[i].ID != y.ToolCalls[i].ID ||
			x.ToolCalls[i].Name != y.ToolCalls[i].Name ||
			x.ToolCalls[i].Arguments != y.ToolCalls[i].Arguments {
			return false
		}
	}
	return true
}

// Touch 更新最近活跃时间。
func (s *Session) Touch() {
	s.LastActiveAt = time.Now()
}

// Persist 将当前会话历史落盘（checkpoint 快照）。
func (s *Session) Persist() error {
	if s.Store == nil {
		return nil
	}
	data := store.SessionData{
		Messages:     s.manager.Raw(),
		SystemPrompt: s.manager.SystemPrompt(),
		TokenUsage:   s.manager.LastUsage(),
		Metadata:     map[string]any{"chat": s.ChatName},
	}
	return s.Store.Save(context.Background(), s.ID, data)
}

// Close 释放会话资源：仅持久化会话历史，不关闭 Provider 连接。
//
// 注意：provider 来自全局缓存（main() 中的 globalProviderCache），多个会话共享
// 同一个 Provider 实例以复用 HTTP 连接池。若在此关闭，会导致其他仍在使用该
// Provider 的会话连接被意外关闭。因此 Provider 的生命周期由 main() 统一管理
// （进程退出时统一 Close），此处不再调用 s.provider.Close()。
func (s *Session) Close() error {
	return s.Persist()
}

// buildSummarizer 构造 compress 模式所需的摘要器，使用给定 Provider 将早期消息压缩为摘要文本。
func buildSummarizer(p provider.Provider) manager.Summarizer {
	return func(msgs []provider.Message) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		var prompt strings.Builder
		prompt.WriteString("请将以下对话历史浓缩为简短摘要，保留关键信息和结论：\n\n")
		for _, m := range msgs {
			if m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0 {
				// assistant 消息可能仅含 tool_calls 而无文本内容，需单独输出工具调用。
				if m.Content != "" {
					prompt.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
				}
				for _, tc := range m.ToolCalls {
					prompt.WriteString(fmt.Sprintf("[%s tool_call]: %s(%s)\n", m.Role, tc.Name, tc.Arguments))
				}
			} else {
				prompt.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
			}
		}

		stream, err := p.Chat(ctx, []provider.Message{
			{Role: provider.RoleUser, Content: prompt.String()},
		}, nil)
		if err != nil {
			return "", fmt.Errorf("摘要请求失败: %w", err)
		}

		var result strings.Builder
		for resp := range stream {
			if resp.Err != nil {
				return "", fmt.Errorf("摘要流式响应错误: %w", resp.Err)
			}
			result.WriteString(resp.Delta.Content)
		}
		return result.String(), nil
	}
}
