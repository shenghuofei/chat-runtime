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
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/web"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// AgentFactory 根据配置与对话名创建一个已注册工具的 Agent，并返回释放资源的清理函数。
//
// 采用工厂函数而非直接构造，便于为每个 WebSocket 会话独立创建 Agent，
// 同时方便在测试中注入 mock 实现。
type AgentFactory func(cfg *config.Config, chatName string) (ag *agent.Agent, cleanup func() error, err error)

// approvalTimeout 是 Web 端审批等待的默认超时。
const approvalTimeout = 5 * time.Minute

// shutdownTimeout 是优雅关闭的最大等待时间。
const shutdownTimeout = 15 * time.Second

// pingInterval 是 WebSocket 心跳发送间隔。
const pingInterval = 30 * time.Second

// pongWait 是等待 pong 响应的超时时间（应大于 pingInterval）。
const pongWait = 45 * time.Second

// writeWait 是 WebSocket 写操作的超时。
const writeWait = 10 * time.Second

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

	// mu 保护对 WebSocket 的并发写与 cancel 的读写。
	mu sync.Mutex
	// cancel 用于取消当前正在进行的生成（type=stop）。
	cancel context.CancelFunc
}

// Server 是 Web 服务的主结构。
type Server struct {
	// cfg 全局配置。
	cfg *config.Config
	// factory 创建 Agent 的工厂函数。
	factory AgentFactory
	// router HTTP 路由。
	router *mux.Router
	// upgrader WebSocket 升级器。
	upgrader websocket.Upgrader
	// logger 日志器。
	logger *log.Logger

	// sessions 活跃会话表，key 为会话 ID。
	sessions   map[string]*Session
	sessionsMu sync.RWMutex
}

// NewServer 根据配置与 Agent 工厂创建一个 Server。
func NewServer(cfg *config.Config, factory AgentFactory) *Server {
	s := &Server{
		cfg:      cfg,
		factory:  factory,
		router:   mux.NewRouter(),
		logger:   log.Default(),
		sessions: make(map[string]*Session),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// 允许跨域连接。生产环境如需限制来源，可在此校验 Origin。
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	s.routes()
	return s
}

// routes 组装 HTTP 路由与中间件。
func (s *Server) routes() {
	// 访问日志中间件对所有路由生效。
	s.router.Use(s.accessLog)

	// 若启用 Basic Auth，则包裹鉴权中间件。
	if s.cfg.Server.BasicAuth.Enabled {
		s.router.Use(s.basicAuth)
	}

	// WebSocket 端点。
	s.router.HandleFunc("/ws", s.handleWebSocket)
	// 列出可用对话预设。
	s.router.HandleFunc("/api/chats", s.handleListChats).Methods(http.MethodGet)
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
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 在独立 goroutine 中启动监听。
	errCh := make(chan error, 1)
	go func() {
		s.logger.Printf("chat-runtime Web 服务已启动: http://%s", addr)
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
		s.logger.Printf("收到信号 %s，开始优雅关闭...", sig)
	}
	signal.Stop(sigCh)

	// 优雅关闭：给活跃请求最多 shutdownTimeout 时间完成。
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		s.logger.Printf("HTTP 服务关闭超时: %v，强制退出", err)
	}

	// 关闭所有 WebSocket 会话（持久化 + 释放 Provider 资源）。
	s.closeAllSessions()
	s.logger.Printf("所有会话已关闭，服务退出")
	return nil
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
		if sess.cleanup != nil {
			_ = sess.cleanup()
		}
	}
}

// handleListChats 返回配置中定义的所有对话预设名称。
func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"chats": s.cfg.ChatNames(),
	})
}

// handleWebSocket 处理 WebSocket 连接的升级与消息循环。
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 会话 ID：优先取查询参数，其次取 Cookie，都没有则新建。
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		if c, err := r.Cookie("session"); err == nil {
			sessionID = c.Value
		}
	}
	if sessionID == "" {
		sessionID = newID()
	}

	// 从查询参数读取对话预设名（缺省为 default）。
	chatName := r.URL.Query().Get("chat")

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("WebSocket 升级失败: %v", err)
		return
	}
	defer conn.Close()

	// 为该连接创建会话，并绑定基于该连接的审批处理器。
	sess := s.createSession(sessionID, chatName, conn)
	if sess == nil {
		_ = conn.WriteJSON(agent.Event{Type: agent.EventError, Content: "创建会话失败"})
		return
	}
	defer s.closeSession(sessionID)

	s.logger.Printf("WebSocket 已连接: session=%s chat=%s", sessionID, chatName)

	// 设置 pong 处理器：收到 pong 时刷新读取超时。
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// 启动心跳 goroutine：定时发送 ping 帧检测连接存活。
	// 当主循环退出（连接关闭）时，pingDone channel 通知心跳停止。
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sess.mu.Lock()
				conn.SetWriteDeadline(time.Now().Add(writeWait))
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
				s.logger.Printf("WebSocket 读取异常: %v", err)
			}
			return
		}
		// 收到有效消息，刷新读取超时。
		conn.SetReadDeadline(time.Now().Add(pongWait))
		s.dispatch(conn, sess, msg)
	}
}

// dispatch 根据消息类型分发处理。
func (s *Server) dispatch(conn *websocket.Conn, sess *Session, msg ClientMessage) {
	switch msg.Type {
	case "chat":
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
	ctx, cancel := context.WithCancel(context.Background())

	// 记录 cancel，供 stop 消息取消当前生成。
	sess.mu.Lock()
	sess.cancel = cancel
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.cancel = nil
		sess.mu.Unlock()
		cancel()
	}()

	if err := sess.agent.Run(ctx, content, func(ev agent.Event) {
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
	conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteJSON(ev); err != nil {
		s.logger.Printf("WebSocket 写入失败: %v", err)
	}
}

// createSession 创建一个新会话并绑定审批处理器。
func (s *Server) createSession(id, chatName string, conn *websocket.Conn) *Session {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	// 已存在同 ID 会话则先关闭旧的（同一会话重连）。
	if old, ok := s.sessions[id]; ok {
		if old.cleanup != nil {
			_ = old.cleanup()
		}
		delete(s.sessions, id)
	}

	ag, cleanup, err := s.factory(s.cfg, chatName)
	if err != nil {
		s.logger.Printf("创建 Agent 失败: %v", err)
		return nil
	}

	sess := &Session{
		ID:      id,
		agent:   ag,
		cleanup: cleanup,
	}

	// 绑定 Web 审批处理器：通过当前连接下发审批请求并等待前端响应。
	sess.approval = agent.NewWSApprovalHandler(func(req agent.ApprovalRequest) error {
		s.send(conn, sess, agent.Event{
			Type:       agent.EventApproval,
			ToolName:   req.ToolName,
			Content:    req.Arguments,
			ApprovalID: req.ID,
		})
		return nil
	}, approvalTimeout)
	ag.SetApprovalHandler(sess.approval)

	s.sessions[id] = sess
	return sess
}

// closeSession 关闭并移除会话，释放其 Agent 资源。
func (s *Server) closeSession(id string) {
	s.sessionsMu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	if ok && sess.cleanup != nil {
		_ = sess.cleanup()
	}
	s.logger.Printf("WebSocket 会话已关闭: session=%s", id)
}

// stop 取消当前正在进行的生成。
func (sess *Session) stop() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cancel != nil {
		sess.cancel()
	}
}

// accessLog 是访问日志中间件，记录请求方法、路径与耗时。
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Printf("%s %s %s %v", r.RemoteAddr, r.Method, r.URL.Path, time.Since(start))
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

// writeJSON 以 JSON 形式写出响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 头部已写出，此处仅记录。
		fmt.Println("writeJSON 编码失败:", err)
	}
}
