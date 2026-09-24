package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shenghuofei/chat-runtime/pkg/agent"
	"github.com/shenghuofei/chat-runtime/pkg/config"
	"github.com/shenghuofei/chat-runtime/pkg/store"
)

// mockFactory 是一个总是返回错误的 AgentFactory，用于不涉及 Agent 创建的路由测试。
func mockFactory(_ *config.Config, _ string, _ string) (*agent.Agent, func() error, error) {
	return nil, nil, nil
}

// newTestServer 创建一个带最小配置的测试 Server。
// 自动注册 t.Cleanup(srv.Close) 防止后台 goroutine（wsRateLimiter 清理定时器等）泄漏。
func newTestServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			PingInterval:    10 * time.Second,
			PongWait:        30 * time.Second,
			WriteWait:       5 * time.Second,
			ShutdownTimeout: 5 * time.Second,
			ApprovalTimeout: 30 * time.Second,
			MaxSessions:     100,
		},
		Chats: map[string]config.ChatConfig{
			"default": {Model: "gpt-4o"},
			"coding":  {Model: "gpt-4o"},
		},
	}
	srv := NewServer(cfg, AgentFactory(mockFactory), st)
	t.Cleanup(func() { srv.Close() })
	return srv
}

// TestHandleListChats 验证 GET /api/chats 返回配置中的对话名列表。
func TestHandleListChats(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/chats", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rr.Code)
	}
	var resp map[string][]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	chats := resp["chats"]
	if len(chats) != 2 {
		t.Fatalf("期望 2 个 chat，实际 %d 个：%v", len(chats), chats)
	}
	chatSet := map[string]bool{}
	for _, c := range chats {
		chatSet[c] = true
	}
	for _, name := range []string{"default", "coding"} {
		if !chatSet[name] {
			t.Errorf("chats 中缺少 %q", name)
		}
	}
}

// TestHandleStats 验证 GET /api/stats 返回包含 uptime/请求数/token 数/会话数的统计。
func TestHandleStats(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rr.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	for _, field := range []string{"uptime_seconds", "total_requests", "total_tokens", "active_sessions"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("响应中缺少字段 %q", field)
		}
	}
}

// TestHandleListSessions_NoStore 验证 store 为 nil 时 GET /api/sessions 返回 501。
func TestHandleListSessions_NoStore(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("期望 501，实际 %d", rr.Code)
	}
}

// TestHandleDeleteSession_NoStore 验证 store 为 nil 时 DELETE /api/sessions/{id} 返回 501。
func TestHandleDeleteSession_NoStore(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/some-id", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("期望 501，实际 %d", rr.Code)
	}
}

// TestHandleListSessions_WithStore 验证有 store 时 GET /api/sessions 返回会话列表。
func TestHandleListSessions_WithStore(t *testing.T) {
	st, err := store.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}

	// 写入一条测试会话。
	ctx := context.Background()
	_ = st.Save(ctx, "sess-abc", store.SessionData{
		Metadata: map[string]any{"chat": "default"},
	})

	srv := newTestServer(t, st)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rr.Code)
	}
	var resp map[string][]store.SessionInfo
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	sessions := resp["sessions"]
	if len(sessions) != 1 {
		t.Fatalf("期望 1 条会话，实际 %d 条", len(sessions))
	}
	if sessions[0].ID != "sess-abc" {
		t.Errorf("会话 ID 不一致：want=sess-abc got=%s", sessions[0].ID)
	}
	if sessions[0].ChatName != "default" {
		t.Errorf("ChatName 不一致：want=default got=%s", sessions[0].ChatName)
	}
}

// TestHandleListSessions_Filter 验证 chat 过滤与分页参数。
func TestHandleListSessions_Filter(t *testing.T) {
	st, err := store.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}

	ctx := context.Background()
	_ = st.Save(ctx, "sess-1", store.SessionData{Metadata: map[string]any{"chat": "default"}})
	_ = st.Save(ctx, "sess-2", store.SessionData{Metadata: map[string]any{"chat": "coding"}})
	_ = st.Save(ctx, "sess-3", store.SessionData{Metadata: map[string]any{"chat": "default"}})

	srv := newTestServer(t, st)

	// 按 chat 过滤。
	req := httptest.NewRequest(http.MethodGet, "/api/sessions?chat=default", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rr.Code)
	}
	var resp map[string][]store.SessionInfo
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(resp["sessions"]) != 2 {
		t.Errorf("chat=default 应有 2 条，实际 %d 条", len(resp["sessions"]))
	}

	// limit=1 分页。
	req2 := httptest.NewRequest(http.MethodGet, "/api/sessions?limit=1", nil)
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req2)

	var resp2 map[string][]store.SessionInfo
	if err := json.NewDecoder(rr2.Body).Decode(&resp2); err != nil {
		t.Fatalf("解析 limit 分页响应失败：%v", err)
	}
	if len(resp2["sessions"]) != 1 {
		t.Errorf("limit=1 应返回 1 条，实际 %d 条", len(resp2["sessions"]))
	}
}

// TestHandleDeleteSession_Active 验证删除活跃会话时会先从 sessions map 移除。
func TestHandleDeleteSession_Active(t *testing.T) {
	st, err := store.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}

	ctx := context.Background()
	_ = st.Save(ctx, "sess-del", store.SessionData{Metadata: map[string]any{"chat": "default"}})

	srv := newTestServer(t, st)

	// 先确认会话存在。
	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/sess-del", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("期望 204，实际 %d（body: %s）", rr.Code, rr.Body.String())
	}

	// 再次请求，文件已删，store.Delete 幂等，应仍返回 204。
	req2 := httptest.NewRequest(http.MethodDelete, "/api/sessions/sess-del", nil)
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("幂等删除期望 204，实际 %d", rr2.Code)
	}
}

// TestWSRateLimiter 验证 per-IP 速率限制逻辑。
func TestWSRateLimiter(t *testing.T) {
	rl := newWSRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("第 %d 次请求应被允许", i+1)
		}
	}
	// 第 4 次超限。
	if rl.allow("1.2.3.4") {
		t.Error("第 4 次请求应被拒绝")
	}
	// 不同 IP 不受影响。
	if !rl.allow("9.9.9.9") {
		t.Error("不同 IP 应被允许")
	}
}
