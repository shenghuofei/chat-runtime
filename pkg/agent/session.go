package agent

import (
	"container/list"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/manager"
	"github.com/shenghuofei/chat-runtime/pkg/provider"
	"github.com/shenghuofei/chat-runtime/pkg/store"
)

// ProviderFactory 根据对话配置创建（或复用）一个 Provider 实例。
//
// 由上层注入，屏蔽“ChatConfig → Model → Provider”的解析细节，
// 使 Session 不直接依赖具体 Provider 的构造过程。
type ProviderFactory func(chatConfig config.ChatConfig) (provider.Provider, error)

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
	p, err := providerFactory(chatConfig)
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
		}
		if logMsgs, err := st.LoadLog(context.Background(), id); err == nil && len(logMsgs) > 0 {
			msgs = append(msgs, logMsgs...)
		}
		if len(msgs) > 0 {
			if verr := manager.ValidateRound(msgs); verr != nil {
				fmt.Fprintf(os.Stderr, "[警告] 会话 %q 历史消息不完整，已尽力恢复: %v\n", id, verr)
			}
			mgr.Load(msgs)
		}
	}

	// 4. 组装 Agent，注入增量持久化回调。
	agCfg := AgentConfig{MaxIterations: chatConfig.MaxIterations}
	if st != nil {
		agCfg.OnMessageAdded = func(msg provider.Message) {
			_ = st.AppendMessage(context.Background(), id, msg)
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
		Messages: s.manager.Raw(),
		Metadata: map[string]any{"chat": s.ChatName},
	}
	return s.Store.Save(context.Background(), s.ID, data)
}

// Close 释放会话资源：持久化历史并关闭 Provider 连接。
func (s *Session) Close() error {
	var firstErr error
	if err := s.Persist(); err != nil {
		firstErr = err
	}
	if s.provider != nil {
		if err := s.provider.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SessionManagerConfig 是 SessionManager 的配置。
type SessionManagerConfig struct {
	// MaxSessions 最大并发会话数（<=0 表示不限制）。超出时按 LRU 淘汰最久未活跃的会话。
	MaxSessions int
	// ProviderFactory 创建 Provider 的工厂。
	ProviderFactory ProviderFactory
	// Store 会话持久化后端（可为 nil，表示不持久化）。
	Store store.Store
	// ChatConfigs 对话配置表，key 为对话名，用于 GetOrCreate 时解析会话配置。
	ChatConfigs map[string]config.ChatConfig
}

// SessionManager 管理多个会话，支持 LRU 淘汰空闲会话。
//
// 内部使用一个双向链表维护 LRU 顺序：表头为最近活跃，表尾为最久未活跃。
type SessionManager struct {
	mu sync.Mutex
	// sessions id -> *list.Element（其 Value 为 *Session）。
	sessions map[string]*list.Element
	// lru 双向链表，Front 最近活跃，Back 最久未活跃。
	lru *list.List
	// config 配置。
	config SessionManagerConfig
}

// NewSessionManager 创建一个 SessionManager。
func NewSessionManager(config SessionManagerConfig) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*list.Element),
		lru:      list.New(),
		config:   config,
	}
}

// GetOrCreate 获取已有会话；不存在则创建。
//
// 每次调用都会把该会话移动到 LRU 表头并刷新活跃时间。
// 创建新会话后若超出 MaxSessions，则淘汰最久未活跃的会话。
func (sm *SessionManager) GetOrCreate(id, chatName string) (*Session, error) {
	sm.mu.Lock()

	// 命中已有会话。
	if elem, ok := sm.sessions[id]; ok {
		sess := elem.Value.(*Session)
		sess.Touch()
		sm.lru.MoveToFront(elem)
		sm.mu.Unlock()
		return sess, nil
	}

	// 解析对话配置。
	chatConfig, ok := sm.config.ChatConfigs[chatName]
	if !ok {
		sm.mu.Unlock()
		return nil, fmt.Errorf("未找到名为 %q 的对话配置", chatName)
	}

	// 创建新会话。
	sess, err := NewSession(id, chatName, chatConfig, sm.config.ProviderFactory, sm.config.Store)
	if err != nil {
		sm.mu.Unlock()
		return nil, err
	}

	elem := sm.lru.PushFront(sess)
	sm.sessions[id] = elem

	// 超出上限则按 LRU 淘汰（收集待淘汰会话，锁外执行 Close）。
	evicted := sm.evictIfNeededLocked()
	sm.mu.Unlock()

	// 在锁外执行可能较慢的 Close（持久化 + 网络关闭），避免阻塞其他请求。
	for _, s := range evicted {
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 淘汰会话 %q 时出错: %v\n", s.ID, err)
		}
	}

	return sess, nil
}

// Get 仅获取已有会话，不存在时返回 (nil, false)。
func (sm *SessionManager) Get(id string) (*Session, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	elem, ok := sm.sessions[id]
	if !ok {
		return nil, false
	}
	sess := elem.Value.(*Session)
	sess.Touch()
	sm.lru.MoveToFront(elem)
	return sess, true
}

// Close 关闭并移除指定会话（持久化并释放资源）。
func (sm *SessionManager) Close(id string) {
	sm.mu.Lock()
	elem, ok := sm.sessions[id]
	if ok {
		delete(sm.sessions, id)
		sm.lru.Remove(elem)
	}
	sm.mu.Unlock()

	if ok {
		// 在锁外执行可能较慢的 Close（持久化 + 网络关闭）。
		_ = elem.Value.(*Session).Close()
	}
}

// CloseAll 关闭所有会话（进程退出时调用）。
func (sm *SessionManager) CloseAll() {
	sm.mu.Lock()
	elems := make([]*list.Element, 0, len(sm.sessions))
	for _, elem := range sm.sessions {
		elems = append(elems, elem)
	}
	sm.sessions = make(map[string]*list.Element)
	sm.lru.Init()
	sm.mu.Unlock()

	for _, elem := range elems {
		s := elem.Value.(*Session)
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 关闭会话 %q 时出错: %v\n", s.ID, err)
		}
	}
}

// buildSummarizer 构造 compress 模式所需的摘要器，使用给定 Provider 将早期消息压缩为摘要文本。
func buildSummarizer(p provider.Provider) manager.Summarizer {
	return func(msgs []provider.Message) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		var prompt strings.Builder
		prompt.WriteString("请将以下对话历史浓缩为简短摘要，保留关键信息和结论：\n\n")
		for _, m := range msgs {
			prompt.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
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

// evictIfNeededLocked 在超出 MaxSessions 时淘汰最久未活跃的会话（调用方需持有锁）。
//
// 收集需要淘汰的会话后，在返回前记录它们，由调用方在释放锁后执行 Close。
func (sm *SessionManager) evictIfNeededLocked() []*Session {
	if sm.config.MaxSessions <= 0 {
		return nil
	}
	var evicted []*Session
	for sm.lru.Len() > sm.config.MaxSessions {
		oldest := sm.lru.Back()
		if oldest == nil {
			break
		}
		sess := oldest.Value.(*Session)
		sm.lru.Remove(oldest)
		delete(sm.sessions, sess.ID)
		evicted = append(evicted, sess)
	}
	return evicted
}
