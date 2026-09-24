// Package server 实现 chat-runtime 的 Web 服务模式（serve 子命令的底层）。
//
// 从 cmd/serve.go 中抽离出来，便于单元测试。核心职责：
//   - 基于 gorilla/mux 组装 HTTP 路由；
//   - 通过 gorilla/websocket 升级 WebSocket 连接并处理消息；
//   - 提供 BasicAuth 与访问日志中间件；
//   - 以会话（Session）为粒度为每个连接创建独立的 Agent。
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/store"
	"github.com/shenghuofei/chat-runtime/pkg/web"
)

// AgentFactory 根据配置、对话名与会话 ID 创建一个已注册工具的 Agent，并返回释放资源的清理函数。
//
// sessionID 为空时由工厂自行生成（CLI 模式稳定 ID，Web 模式随机 ID）；
// 非空时工厂使用该 ID 加载对应的历史会话（Web 模式 Resume 场景）。
//
// 采用工厂函数而非直接构造，便于为每个 WebSocket 会话独立创建 Agent，
// 同时方便在测试中注入 mock 实现。
type AgentFactory func(cfg *config.Config, chatName string, sessionID string) (ag *agent.Agent, cleanup func() error, err error)

// Server 级别的超时均从 cfg.Server 读取，在 config.applyDefaults 中已填入合理默认值。
// 这里保留常量仅作文档说明，不再被实际代码引用。

// newID 生成一个随机的十六进制标识（用于会话 ID）。
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 极少失败；退化为基于时间的标识以保证可用性。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ClientMessage 是前端通过 WebSocket 发来的消息。
type ClientMessage struct {
	// Type 消息类型：chat / stop / clear / approval。
	Type string `json:"type"`
	// Content 用户输入内容（type=chat 时使用）。
	Content string `json:"content,omitempty"`
	// Approved 审批结果（type=approval 时使用）。
	Approved bool `json:"approved,omitempty"`
	// ApprovalID 审批请求标识（type=approval 时使用）。
	ApprovalID string `json:"approval_id,omitempty"`
}

// Session 表示一个 WebSocket 会话，持有独立的 Agent 与审批处理器。
type Session struct {
	// ID 会话唯一标识。
	ID string
	// agent 该会话绑定的 Agent 实例。
	agent *agent.Agent
	// cleanup 释放 Agent 资源的回调。
	cleanup func() error
	// approval Web 审批处理器，负责下发审批请求与匹配响应。
	approval *agent.WSApprovalHandler

	// lastActiveAtNano 会话最近活跃时间（UnixNano），用于 LRU 淘汰时选出空闲最久的会话。
	// 创建时初始化为当前时间，每次收到 chat 消息时更新。
	// 使用 atomic.Int64 避免在 evictOldestSessionLocked（仅持 sessionsMu）中
	// 读取与 dispatch（持 sess.mu）写入之间的数据竞争。
	lastActiveAtNano atomic.Int64

	// msgRL 单会话消息速率限制器（每分钟最多 60 条 chat 消息）。
	msgRL *msgRateLimiter

	// runMu 串行化同一会话内的多个并发生成：
	// 用户快速连发两条 chat 消息时，dispatch 会启动两个 runChat goroutine。
	// 第二个 goroutine 等待第一个完成后才开始执行，避免两个 agent.Run 并发写入
	// manager，导致消息顺序错乱（两条 user 消息交错追加到历史）。
	runMu sync.Mutex

	// mu 保护对 WebSocket 的并发写与 cancel 的读写。
	mu sync.Mutex
	// cancel 用于取消当前正在进行的生成（type=stop）。
	cancel context.CancelFunc
}

// wsRateLimiter 基于固定时间窗口的 per-IP WebSocket 连接速率限制器。
// 在每个时间窗口内，同一 IP 最多允许建立 maxPerWindow 个新连接。
type wsRateLimiter struct {
	mu           sync.Mutex
	entries      map[string]*wsRLEntry
	maxPerWindow int
	window       time.Duration
}

type wsRLEntry struct {
	count     int
	windowEnd time.Time
}

func newWSRateLimiter(maxPerWindow int, window time.Duration) *wsRateLimiter {
	return &wsRateLimiter{
		entries:      make(map[string]*wsRLEntry),
		maxPerWindow: maxPerWindow,
		window:       window,
	}
}

// allow 检查 ip 是否允许建立新连接，同时更新计数。
// 超出窗口期的旧条目在下次访问时懒惰清理。
func (rl *wsRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	entry, ok := rl.entries[ip]
	if !ok || now.After(entry.windowEnd) {
		rl.entries[ip] = &wsRLEntry{count: 1, windowEnd: now.Add(rl.window)}
		return true
	}
	if entry.count >= rl.maxPerWindow {
		return false
	}
	entry.count++
	return true
}

// cleanupExpired 清除所有已过期的 IP 条目，防止长期运行后内存中积累大量废旧条目。
func (rl *wsRateLimiter) cleanupExpired() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for ip, e := range rl.entries {
		if now.After(e.windowEnd) {
			delete(rl.entries, ip)
		}
	}
}

// msgRateLimiter 是单个 WebSocket 会话内的消息速率限制器（固定时间窗口）。
// 防止已连接的客户端通过高频 chat 消息触发大量 LLM API 调用。
type msgRateLimiter struct {
	mu           sync.Mutex
	count        int
	windowEnd    time.Time
	maxPerWindow int
	window       time.Duration
}

// allow 检查当前会话是否允许再发送一条消息。
func (ml *msgRateLimiter) allow() bool {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	now := time.Now()
	if now.After(ml.windowEnd) {
		ml.count = 1
		ml.windowEnd = now.Add(ml.window)
		return true
	}
	if ml.count >= ml.maxPerWindow {
		return false
	}
	ml.count++
	return true
}

// Server 是 Web 服务的主结构。
type Server struct {
	// cfg 全局配置。
	cfg *config.Config
	// factory 创建 Agent 的工厂函数。
	factory AgentFactory
	// store 会话持久化后端，用于 ListSessions / Delete 等管理 API。
	store store.Store
	// router HTTP 路由。
	router *mux.Router
	// upgrader WebSocket 升级器。
	upgrader websocket.Upgrader
	// logger 结构化日志器。
	logger *slog.Logger
	// wsRateLimiter per-IP WebSocket 连接速率限制器。
	wsRateLimiter *wsRateLimiter

	// sessions 活跃会话表，key 为会话 ID。
	sessions   map[string]*Session
	sessionsMu sync.RWMutex

	// done 关闭信号 channel：Close() 或 Start() 完成后关闭，
	// 通知后台 goroutine（如 wsRateLimiter 清理定时器）退出，防止测试中泄漏。
	done chan struct{}

	// stats 服务运行统计。
	stats struct {
		totalRequests atomic.Int64
		totalTokens   atomic.Int64
		startTime     time.Time
	}
}

// NewServer 根据配置、Agent 工厂与 Store 创建一个 Server。
// store 用于会话管理 API（列出/删除会话），可为 nil（此时管理 API 返回 501）。
func NewServer(cfg *config.Config, factory AgentFactory, st store.Store) *Server {
	s := &Server{
		cfg:           cfg,
		factory:       factory,
		store:         st,
		router:        mux.NewRouter(),
		logger:        slog.Default(),
		sessions:      make(map[string]*Session),
		done:          make(chan struct{}),
		wsRateLimiter: newWSRateLimiter(10, time.Minute), // 每 IP 每分钟最多 10 次新连接
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// CheckOrigin 防御跨站 WebSocket 劫持（CSWSH）：
			//   - 启用 BasicAuth 时，鉴权已保护连接，允许所有 Origin；
			//   - 未启用时，校验 Origin 的 host 与请求 Host 一致，拒绝跨源连接。
			CheckOrigin: func(r *http.Request) bool {
				if cfg.Server.BasicAuth.Enabled {
					return true // 鉴权已保护
				}
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // 非浏览器客户端（无 Origin 头）
				}
				// 简单校验：Origin 的 host 部分应与请求 Host 一致。
				if u, err := url.Parse(origin); err == nil {
					return u.Host == r.Host
				}
				return false
			},
		},
	}
	s.stats.startTime = time.Now()
	s.routes()

	// 每 5 分钟清理 wsRateLimiter 中已过期的 IP 条目，
	// 防止长期运行后内存中积累大量废旧条目（每个条目约 40 字节，通常可忽略，
	// 但 DoS 场景下大量不同 IP 连接会使 map 持续增长）。
	// 通过 s.done channel 感知 Server 关闭，避免测试中 goroutine 泄漏。
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.wsRateLimiter.cleanupExpired()
			case <-s.done:
				return
			}
		}
	}()

	return s
}

// routes 组装 HTTP 路由与中间件。
func (s *Server) routes() {
	// 访问日志中间件对所有路由生效。
	s.router.Use(s.accessLog)

	// 若配置了 CORS 白名单，则启用 CORS 中间件。
	if len(s.cfg.Server.AllowedOrigins) > 0 {
		s.router.Use(s.corsMiddleware)
	}

	// 若启用 Basic Auth，则包裹鉴权中间件。
	if s.cfg.Server.BasicAuth.Enabled {
		s.router.Use(s.basicAuth)
	}

	// WebSocket 端点。
	s.router.HandleFunc("/ws", s.handleWebSocket)
	// 列出可用对话预设。
	s.router.HandleFunc("/api/chats", s.handleListChats).Methods(http.MethodGet)
	// 会话管理 API。
	s.router.HandleFunc("/api/sessions", s.handleListSessions).Methods(http.MethodGet)
	s.router.HandleFunc("/api/sessions/{id}", s.handleDeleteSession).Methods(http.MethodDelete)
	// 服务运行统计。
	s.router.HandleFunc("/api/stats", s.handleStats).Methods(http.MethodGet)
	// 静态资源（内嵌的前端），置于最后作为兜底路由。
	s.router.PathPrefix("/").Handler(web.FileServer())
}

// Handler 返回底层 http.Handler，便于测试中直接使用 httptest。
func (s *Server) Handler() http.Handler {
	return s.router
}

// Start 在指定地址启动 HTTP 服务。
//
// 支持 graceful shutdown：监听 SIGINT / SIGTERM 信号后，先停止接收新连接，
// 等待活跃请求在 shutdownTimeout 内完成，再关闭所有会话并退出。
func (s *Server) Start(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadHeaderTimeout: s.cfg.Server.ReadHeaderTimeout,
	}

	// 在独立 goroutine 中启动监听。
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("chat-runtime Web 服务已启动", "addr", "http://"+addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// 等待退出信号。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		// 监听失败（端口占用等），直接返回错误。
		return err
	case sig := <-sigCh:
		s.logger.Info("收到信号，开始优雅关闭", "signal", sig)
	}
	signal.Stop(sigCh)

	// 优雅关闭：给活跃请求最多 ShutdownTimeout 时间完成。
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		s.logger.Warn("HTTP 服务关闭超时，强制关闭", "error", err)
		_ = srv.Close()
	}

	// 关闭所有 WebSocket 会话（持久化 + 释放 Provider 资源）。
	s.closeAllSessions()
	// 关闭后台 goroutine（wsRateLimiter 清理定时器等）。
	s.Close()
	s.logger.Info("所有会话已关闭，服务退出")
	return nil
}

// Close 停止 Server 的后台 goroutine（如 wsRateLimiter 清理定时器）。
//
// 幂等：重复调用安全。测试中应在 NewServer 后 defer s.Close() 防止 goroutine 泄漏。
// Start() 完成时会自动调用，生产场景无需手动调用。
func (s *Server) Close() {
	select {
	case <-s.done:
		// 已关闭，幂等返回。
	default:
		close(s.done)
	}
}

// closeAllSessions 关闭所有活跃的 WebSocket 会话。
func (s *Server) closeAllSessions() {
	s.sessionsMu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessions = make(map[string]*Session)
	s.sessionsMu.Unlock()

	for _, sess := range sessions {
		// 先取消正在进行的生成，再清理资源，避免 cleanup（持久化）与仍在运行的
		// runChat goroutine 之间产生写入竞争。
		sess.stop()
		// 等待 runChat 释放 runMu，确保所有消息已追加到 store 后再 Persist/Release。
		sess.runMu.Lock()
		sess.runMu.Unlock() //nolint:staticcheck
		if sess.cleanup != nil {
			if err := sess.cleanup(); err != nil {
				s.logger.Warn("关闭会话失败", "session", sess.ID, "error", err)
			}
		}
	}
}

// handleListChats 返回配置中定义的所有对话预设名称。
func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"chats": s.cfg.ChatNames(),
	})
}

// handleWebSocket 处理 WebSocket 连接的升级与消息循环。
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// per-IP 速率限制：防止恶意客户端快速建立大量连接触发大量 LLM API 调用。
	// 取 RemoteAddr 中的 IP 部分（格式为 "ip:port"）。
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	if !s.wsRateLimiter.allow(ip) {
		http.Error(w, "连接过于频繁，请稍后重试", http.StatusTooManyRequests)
		return
	}

	// Session ID 决策：
	//   - resume_session_id 非空：恢复指定历史会话（工厂使用该 ID 加载对应的历史）；
	//   - 否则：由服务端生成新的随机 ID，防止客户端劫持或猜测他人会话。
	// 注意：单用户部署场景下 resume_session_id 无需鉴权保护，
	// 多用户场景应在 BasicAuth 或更上层实现 session 隔离。
	var sessionID string
	if resumeID := r.URL.Query().Get("resume_session_id"); resumeID != "" {
		// 防止路径穿越：拒绝含路径分隔符或 ".." 的 ID，避免攻击者构造
		// 如 "../../etc/passwd" 的 ID 访问非预期文件。
		if strings.ContainsAny(resumeID, "/\\") || strings.Contains(resumeID, "..") {
			http.Error(w, "无效的 session_id", http.StatusBadRequest)
			return
		}
		sessionID = resumeID
	} else {
		sessionID = newID()
	}

	// 仅保留对话预设名（chat name）参数（缺省为 default）。
	chatName := r.URL.Query().Get("chat")

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// 升级失败通常是客户端协议不合规或网络问题，使用 Error 级别以便运维及时发现。
		s.logger.Error("WebSocket 升级失败", "remote", r.RemoteAddr, "error", err)
		return
	}
	defer conn.Close()
	// 限制单条消息最大 8 MB，防止客户端发送超大消息耗尽内存。
	conn.SetReadLimit(8 * 1024 * 1024)

	// 升级成功后立即将服务端生成的 session ID 通过第一条消息告知客户端。
	_ = conn.WriteJSON(map[string]string{"type": "session", "session_id": sessionID})

	// 为该连接创建会话，并绑定基于该连接的审批处理器。
	sess := s.createSession(sessionID, chatName, conn)
	if sess == nil {
		_ = conn.WriteJSON(agent.Event{Type: agent.EventError, Content: "创建会话失败"})
		return
	}
	defer s.closeSession(sessionID)

	s.logger.Info("WebSocket 已连接", "session", sessionID, "chat", chatName)

	// 设置 pong 处理器：收到 pong 时刷新读取超时。
	conn.SetReadDeadline(time.Now().Add(s.cfg.Server.PongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(s.cfg.Server.PongWait))
		return nil
	})

	// 启动心跳 goroutine：定时发送 ping 帧检测连接存活。
	// 当主循环退出（连接关闭）时，pingDone channel 通知心跳停止。
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(s.cfg.Server.PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sess.mu.Lock()
				conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteWait))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				sess.mu.Unlock()
				if err != nil {
					return // 写入失败，连接已断
				}
			case <-pingDone:
				return
			}
		}
	}()
	defer close(pingDone)

	// 消息读取循环。
	for {
		var msg ClientMessage
		if err := conn.ReadJSON(&msg); err != nil {
			// 连接关闭或读取错误，结束循环。
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				s.logger.Warn("WebSocket 读取异常", "error", err)
			}
			return
		}
		// 收到有效消息，刷新读取超时。
		conn.SetReadDeadline(time.Now().Add(s.cfg.Server.PongWait))
		s.dispatch(conn, sess, msg)
	}
}

// dispatch 根据消息类型分发处理。
func (s *Server) dispatch(conn *websocket.Conn, sess *Session, msg ClientMessage) {
	switch msg.Type {
	case "chat":
		// per-session 消息速率限制：防止单会话高频发消息触发大量 LLM API 调用。
		if !sess.msgRL.allow() {
			s.send(conn, sess, agent.Event{Type: agent.EventError, Content: "消息发送过于频繁，请稍后重试"})
			return
		}
		// 更新会话最近活跃时间，用于 LRU 淘汰空闲最久的会话。
		sess.lastActiveAtNano.Store(time.Now().UnixNano())
		// 新的一轮对话：异步执行，避免阻塞读取循环（以便处理 stop / approval）。
		go s.runChat(conn, sess, msg.Content)
	case "stop":
		// 停止当前生成。
		sess.stop()
	case "clear":
		// 清空历史。
		sess.agent.Clear()
		s.send(conn, sess, agent.Event{Type: agent.EventDone})
	case "approval":
		// 审批回复：唤醒对应的等待 goroutine。
		sess.approval.Resolve(msg.ApprovalID, msg.Approved)
	default:
		s.send(conn, sess, agent.Event{Type: agent.EventError, Content: "未知消息类型: " + msg.Type})
	}
}

// runChat 执行一轮对话生成，将事件流式推送回前端。
func (s *Server) runChat(conn *websocket.Conn, sess *Session, content string) {
	// 串行化：同一会话同时只能有一个生成在运行。
	// 用户快速连发两条消息时，dispatch 会启动两个 goroutine，第二个在此等待，
	// 避免两个 agent.Run 并发追加消息导致历史乱序。
	sess.runMu.Lock()
	defer sess.runMu.Unlock()

	s.stats.totalRequests.Add(1)

	ctx, cancel := context.WithCancel(context.Background())

	// 取消上一轮未完成的生成（若有），再登记新的 cancel。
	sess.mu.Lock()
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.cancel = cancel
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.cancel = nil
		sess.mu.Unlock()
		cancel()
	}()

	if err := sess.agent.Run(ctx, content, func(ev agent.Event) {
		if ev.Type == agent.EventDone && ev.Usage != nil {
			s.stats.totalTokens.Add(int64(ev.Usage.TotalTokens))
		}
		s.send(conn, sess, ev)
	}); err != nil {
		if ctx.Err() != nil {
			// 用户主动停止，不作为错误上报。
			return
		}
		s.send(conn, sess, agent.Event{Type: agent.EventError, Content: err.Error()})
		return
	}
}

// send 线程安全地向 WebSocket 写入一个事件。
func (s *Server) send(conn *websocket.Conn, sess *Session, ev agent.Event) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteWait))
	if err := conn.WriteJSON(ev); err != nil {
		s.logger.Warn("WebSocket 写入失败", "error", err)
	}
}

// createSession 创建一个新会话并绑定审批处理器。
//
// 耗时的 Agent 初始化（连接 MCP、创建 HTTP 客户端等）在锁外进行，
// 避免阻塞其他并发的 WebSocket 连接建立。
func (s *Server) createSession(id, chatName string, conn *websocket.Conn) *Session {
	// 会话数上限的处理策略（修复 10）：不再在此直接拒绝新连接，
	// 而是在写回 sessions map 时（步骤 3）按 LRU 淘汰最旧的会话，
	// 为新会话腾出位置。这样既能防止会话表无限增长导致内存泄漏，
	// 又能保证新连接始终可用（避免达到上限后彻底无法建连）。

	// 1. 取出并移除同 ID 的旧会话（如有），在锁外执行 cleanup 避免持锁期间做网络操作。
	s.sessionsMu.Lock()
	old, hadOld := s.sessions[id]
	if hadOld {
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	if hadOld {
		// 先取消正在进行的生成，再执行资源清理，与 closeSession 保持一致。
		old.stop()
		if old.cleanup != nil {
			_ = old.cleanup()
		}
	}

	// 2. 锁外执行耗时的 Agent 创建（MCP 握手、HTTP 连接等）。
	// 将 sessionID 传入工厂：工厂将以此 ID 加载历史（Resume）或创建新历史。
	ag, cleanup, err := s.factory(s.cfg, chatName, id)
	if err != nil {
		s.logger.Error("创建 Agent 失败", "error", err)
		return nil
	}

	sess := &Session{
		ID:      id,
		agent:   ag,
		cleanup: cleanup,
		// 每分钟最多 60 条 chat 消息（平均 1 条/秒），防止单会话高频刷请求。
		msgRL: &msgRateLimiter{maxPerWindow: 60, window: time.Minute},
	}
	sess.lastActiveAtNano.Store(time.Now().UnixNano())

	// 绑定 Web 审批处理器：通过当前连接下发审批请求并等待前端响应。
	sess.approval = agent.NewWSApprovalHandler(func(req agent.ApprovalRequest) error {
		s.send(conn, sess, agent.Event{
			Type:       agent.EventApproval,
			ToolName:   req.ToolName,
			Content:    req.Arguments,
			ApprovalID: req.ID,
		})
		return nil
	}, s.cfg.Server.ApprovalTimeout)
	ag.SetApprovalHandler(sess.approval)

	// 3. 写回 sessions map（二次检查：极低概率下同一 ID 被并发创建，后者胜出）。
	s.sessionsMu.Lock()
	if existing, ok := s.sessions[id]; ok {
		// 另一个 goroutine 已创建同 ID 会话，丢弃刚建好的，返回已有的。
		s.sessionsMu.Unlock()
		if cleanup != nil {
			_ = cleanup()
		}
		return existing
	}
	// 超出最大会话数时，淘汰最旧的会话，避免会话表无限增长导致内存泄漏。
	// 注：本会话使用的是当前 ID（不在 map 中），因此当 count >= MaxSessions 时需要先腾出一个位置。
	if s.cfg.Server.MaxSessions > 0 && len(s.sessions) >= s.cfg.Server.MaxSessions {
		s.evictOldestSessionLocked()
	}
	s.sessions[id] = sess
	s.sessionsMu.Unlock()

	return sess
}

// evictOldestSessionLocked 淘汰创建时间最早的会话（调用方必须持有 sessionsMu 写锁）。
//
// 为避免在持锁期间执行可能阻塞的 cleanup（持久化 / 网络操作），
// 本方法先从 map 中移除最旧会话，随后在独立 goroutine 中异步清理其资源。
func (s *Server) evictOldestSessionLocked() {
	var oldest *Session
	for _, sess := range s.sessions {
		if oldest == nil || sess.lastActiveAtNano.Load() < oldest.lastActiveAtNano.Load() {
			oldest = sess
		}
	}
	if oldest == nil {
		return
	}
	delete(s.sessions, oldest.ID)
	s.logger.Info("会话数超限，淘汰空闲最久的会话", "session", oldest.ID, "max_sessions", s.cfg.Server.MaxSessions)

	// 异步清理被淘汰会话的资源，避免持锁期间阻塞。
	go func(sess *Session) {
		sess.stop()
		// 等待正在进行的 runChat 持有的 runMu 被释放，再执行持久化清理。
		// 若不等待，cleanup（Persist/Release）可能与仍在写入 manager/store 的
		// runChat goroutine 并发执行，导致 store.Release() 关闭写入器后
		// runChat 的 AppendMessage 无法落盘，末尾消息丢失。
		sess.runMu.Lock()
		//nolint:staticcheck // 仅用于等待 runChat 退出，不持有锁执行任何操作
		sess.runMu.Unlock()
		// 检查该 sessionID 是否已被重新加入 map（同 ID 断线重连/恢复），
		// 若是则跳过 cleanup，避免关闭新会话的 store 写入器。
		s.sessionsMu.RLock()
		_, reAdded := s.sessions[sess.ID]
		s.sessionsMu.RUnlock()
		if reAdded {
			return
		}
		if sess.cleanup != nil {
			if err := sess.cleanup(); err != nil {
				s.logger.Warn("淘汰会话时清理资源出错", "session", sess.ID, "error", err)
			}
		}
	}(oldest)
}

// closeSession 关闭并移除会话，释放其 Agent 资源。
func (s *Server) closeSession(id string) {
	s.sessionsMu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	if ok {
		// 先取消正在进行的生成：WebSocket 断开后 runChat goroutine 若仍在等待 LLM
		// 响应，不取消则它会一直运行到响应返回，无法被服务端优雅关闭感知。
		sess.stop()
		// 等待 runChat 释放 runMu 后再执行 cleanup，防止 store.Release 关闭写入器
		// 后 runChat 的 AppendMessage 无法落盘。
		sess.runMu.Lock()
		sess.runMu.Unlock() //nolint:staticcheck
		if sess.cleanup != nil {
			if err := sess.cleanup(); err != nil {
				s.logger.Warn("关闭会话时出错", "session", id, "error", err)
			}
		}
	}
	s.logger.Info("WebSocket 会话已关闭", "session", id)
}

// stop 取消当前正在进行的生成。
func (sess *Session) stop() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cancel != nil {
		sess.cancel()
	}
}

// responseRecorder 包装 http.ResponseWriter，捕获实际写出的 HTTP 状态码。
type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (rr *responseRecorder) WriteHeader(status int) {
	rr.status = status
	rr.ResponseWriter.WriteHeader(status)
}

// accessLog 是访问日志中间件，记录请求方法、路径、响应状态码与耗时。
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.Info("HTTP 请求",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}

// basicAuth 是 HTTP Basic Auth 中间件。
//
// 使用 subtle.ConstantTimeCompare 进行常量时间比较，避免时序侧信道攻击。
func (s *Server) basicAuth(next http.Handler) http.Handler {
	ba := s.cfg.Server.BasicAuth
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !secureCompare(user, ba.Username) || !secureCompare(pass, ba.Password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="chat-runtime"`)
			http.Error(w, "未授权", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secureCompare 常量时间比较两个字符串是否相等。
func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// corsMiddleware 是 CORS 中间件，按 AllowedOrigins 配置设置跨域响应头。
//
// 支持精确 Origin 匹配与通配符 "*"；对 OPTIONS 预检请求直接返回 204，
// 不转发给后续处理器，避免因鉴权中间件拒绝预检请求而导致 CORS 握手失败。
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	allowed := s.cfg.Server.AllowedOrigins
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			for _, o := range allowed {
				if o == "*" || o == origin {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					break
				}
			}
		}
		// OPTIONS 预检请求在此终止，不往后传递（避免鉴权中间件拦截）。
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleStats 返回服务运行统计信息。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds":  time.Since(s.stats.startTime).Seconds(),
		"total_requests":  s.stats.totalRequests.Load(),
		"total_tokens":    s.stats.totalTokens.Load(),
		"active_sessions": s.sessionCount(),
	})
}

// sessionCount 返回当前活跃会话数量。
func (s *Server) sessionCount() int {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return len(s.sessions)
}

// writeJSON 以 JSON 形式写出响应。
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 头部已写出，此处仅记录。
		s.logger.Warn("writeJSON 编码失败", "error", err)
	}
}

// handleListSessions 列出持久化的会话摘要。
//
// GET /api/sessions?chat=xxx&limit=20&offset=0
//
// 可选参数：
//   - chat：按对话预设名过滤；
//   - limit：返回数量上限，默认不限制；
//   - offset：分页偏移量，默认 0。
//
// 返回按最后修改时间降序排列的会话列表。
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "会话管理不可用", http.StatusNotImplemented)
		return
	}

	q := r.URL.Query()
	filter := store.ListFilter{
		Chat: q.Get("chat"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			filter.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			filter.Offset = n
		}
	}

	infos, err := s.store.ListSessions(r.Context(), filter)
	if err != nil {
		s.logger.Error("列出会话失败", "error", err)
		http.Error(w, "列出会话失败", http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"sessions": infos})
}

// handleDeleteSession 删除指定的持久化会话。
//
// DELETE /api/sessions/{id}
//
// 若该会话当前活跃（有 WebSocket 连接），先关闭连接再删除文件。
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "会话管理不可用", http.StatusNotImplemented)
		return
	}

	id := mux.Vars(r)["id"]
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}
	// 防路径穿越：与 WebSocket 升级时的 session_id 校验保持一致。
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		http.Error(w, "无效的会话 ID", http.StatusBadRequest)
		return
	}

	// 原子地检查并移除活跃会话（持写锁，避免检查与删除之间的 TOCTOU 窗口）。
	s.sessionsMu.Lock()
	sess, active := s.sessions[id]
	if active {
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	// 在锁外清理资源，避免持锁期间执行阻塞 I/O（持久化、网络等）。
	if active {
		sess.stop()
		if sess.cleanup != nil {
			if err := sess.cleanup(); err != nil {
				s.logger.Warn("关闭活跃会话时出错", "session", id, "error", err)
			}
		}
	}

	// 删除持久化文件。
	if err := s.store.Delete(r.Context(), id); err != nil {
		s.logger.Error("删除会话失败", "session", id, "error", err)
		http.Error(w, "删除会话失败", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
