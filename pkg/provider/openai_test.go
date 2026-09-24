package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// collectStream 从流式 channel 中收集所有响应，直到 channel 关闭。
// 返回：拼接后的完整文本、累积的工具调用、最终用量、遇到的第一个错误。
func collectStream(t *testing.T, ch <-chan StreamResponse) (content string, toolCalls []ToolCall, usage *TokenUsage, firstErr error) {
	t.Helper()
	var sb strings.Builder
	for sr := range ch {
		if sr.Err != nil && firstErr == nil {
			firstErr = sr.Err
		}
		sb.WriteString(sr.Delta.Content)
		if len(sr.Delta.ToolCalls) > 0 {
			toolCalls = sr.Delta.ToolCalls
		}
		if sr.Usage != nil {
			usage = sr.Usage
		}
	}
	return sb.String(), toolCalls, usage, firstErr
}

// newTestProvider 基于给定 mock server 创建一个测试用 Provider。
func newTestProvider(t *testing.T, srv *httptest.Server) Provider {
	t.Helper()
	p, err := NewOpenAIProvider(ProviderConfig{
		Type:    "openai",
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Model:   "gpt-test",
	})
	if err != nil {
		t.Fatalf("创建 Provider 失败: %v", err)
	}
	return p
}

// writeSSE 向响应写入一行 SSE 数据并立即 flush。
func writeSSE(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestSSEStreamParsing 验证 SSE 文本流能被正确解析并拼接。
func TestSSEStreamParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 分多个数据块返回文本增量。
		writeSSE(w, `{"choices":[{"delta":{"content":"你好"},"finish_reason":""}]}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"，世界"},"finish_reason":""}]}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"！"},"finish_reason":"stop"}]}`)
		// 末尾单独返回用量统计。
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		writeSSE(w, "[DONE]")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	defer p.Close()

	ch, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}

	content, _, usage, streamErr := collectStream(t, ch)
	if streamErr != nil {
		t.Fatalf("流式过程出现错误: %v", streamErr)
	}
	if want := "你好，世界！"; content != want {
		t.Errorf("拼接内容不匹配: 期望 %q, 实际 %q", want, content)
	}
	if usage == nil {
		t.Fatal("期望收到用量统计，实际为 nil")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Errorf("用量统计不匹配: %+v", usage)
	}
}

// TestToolCallExtraction 验证分片的工具调用能被正确累积与提取。
func TestToolCallExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 第一个分片：包含 id 与 name，参数开始。
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":""}]}`)
		// 第二个分片：仅包含参数的后半部分。
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":""}]}`)
		// 结束块：finish_reason 为 tool_calls，触发工具调用输出。
		writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, "[DONE]")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	defer p.Close()

	tools := []ToolDef{{
		Name:        "get_weather",
		Description: "查询天气",
		Parameters:  map[string]interface{}{"type": "object"},
	}}
	ch, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "北京天气"}}, tools)
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}

	_, toolCalls, _, streamErr := collectStream(t, ch)
	if streamErr != nil {
		t.Fatalf("流式过程出现错误: %v", streamErr)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("期望 1 个工具调用，实际 %d 个", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc.ID != "call_1" {
		t.Errorf("工具调用 ID 不匹配: 期望 call_1, 实际 %q", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("工具名不匹配: 期望 get_weather, 实际 %q", tc.Name)
	}
	if want := `{"city":"北京"}`; tc.Arguments != want {
		t.Errorf("工具参数拼接不匹配: 期望 %q, 实际 %q", want, tc.Arguments)
	}
}

// TestClientErrorNoRetry 验证 4xx 错误不重试且立即返回错误。
func TestClientErrorNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized) // 401
		fmt.Fprint(w, `{"error":{"message":"无效的 API Key","type":"auth_error"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	defer p.Close()

	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("期望 4xx 返回错误，实际为 nil")
	}
	if !strings.Contains(err.Error(), "无效的 API Key") {
		t.Errorf("错误信息未包含服务端消息: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("4xx 不应重试: 期望调用 1 次, 实际 %d 次", got)
	}
}

// TestServerErrorRetry 验证 5xx 错误会重试，并在最终成功时返回结果。
func TestServerErrorRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		// 前两次返回 500，第三次成功。
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"内部错误"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		writeSSE(w, "[DONE]")
	}))
	defer srv.Close()

	// 使用较短的重试延迟以加速测试。
	p, err := NewOpenAIProvider(ProviderConfig{
		Type: "openai", APIKey: "k", BaseURL: srv.URL, Model: "m",
	})
	if err != nil {
		t.Fatalf("创建 Provider 失败: %v", err)
	}
	defer p.Close()

	ch, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("重试后应成功，实际返回错误: %v", err)
	}
	content, _, _, streamErr := collectStream(t, ch)
	if streamErr != nil {
		t.Fatalf("流式过程出现错误: %v", streamErr)
	}
	if content != "ok" {
		t.Errorf("内容不匹配: 期望 ok, 实际 %q", content)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("期望重试至第 3 次成功, 实际调用 %d 次", got)
	}
}

// TestServerErrorRetryExhausted 验证持续 5xx 时重试耗尽后返回错误。
func TestServerErrorRetryExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway) // 502
		fmt.Fprint(w, `{"error":{"message":"网关错误"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	defer p.Close()

	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("期望重试耗尽后返回错误，实际为 nil")
	}
	// 总调用次数 = 首次 + maxRetries 次重试。
	if got := atomic.LoadInt32(&calls); got != int32(maxRetries+1) {
		t.Errorf("期望调用 %d 次, 实际 %d 次", maxRetries+1, got)
	}
}

// TestContextCancellation 验证 context 取消时能及时停止消费流。
func TestContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 持续缓慢地发送数据，直到连接断开。
		for i := 0; i < 100; i++ {
			writeSSE(w, `{"choices":[{"delta":{"content":"x"},"finish_reason":""}]}`)
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Chat(ctx, []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}

	// 读取少量数据后取消。
	<-ch
	cancel()

	// 取消后 channel 应在有限时间内关闭。
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
		// 正常关闭。
	case <-time.After(2 * time.Second):
		t.Fatal("context 取消后 channel 未在预期时间内关闭")
	}
}

// TestFactoryRegistration 验证内置供应商均已注册且可被工厂创建。
func TestFactoryRegistration(t *testing.T) {
	for _, typ := range []string{"openai", "deepseek", "ollama", "ark", "qwen", "claude", "gemini"} {
		p, err := New(ProviderConfig{Type: typ, APIKey: "k", Model: "m"})
		if err != nil {
			t.Errorf("类型 %q 创建失败: %v", typ, err)
			continue
		}
		if p.Name() != typ {
			t.Errorf("类型 %q 的 Name() 不匹配: 实际 %q", typ, p.Name())
		}
		_ = p.Close()
	}
}

// TestFactoryUnknownType 验证未知类型返回错误。
func TestFactoryUnknownType(t *testing.T) {
	_, err := New(ProviderConfig{Type: "unknown-xyz"})
	if err == nil {
		t.Fatal("期望未知类型返回错误，实际为 nil")
	}
}

// ---- convertMessages 与 convertTools 单元测试 ----

// TestConvertMessagesBasic 验证各角色消息被正确映射为 chatMessage。
func TestConvertMessagesBasic(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "你是助手"},
		{Role: RoleUser, Content: "你好"},
		{Role: RoleAssistant, Content: "你好！"},
		{Role: RoleTool, Content: "工具结果", ToolCallID: "call_1", Name: "run"},
	}
	out := convertMessages(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("消息数量不一致：want=%d got=%d", len(msgs), len(out))
	}
	for i, m := range msgs {
		if out[i].Role != string(m.Role) {
			t.Errorf("[%d] Role: want=%q got=%q", i, m.Role, out[i].Role)
		}
		if out[i].Content != m.Content {
			t.Errorf("[%d] Content: want=%q got=%q", i, m.Content, out[i].Content)
		}
	}
	// tool 消息的 ToolCallID 与 Name 应原样保留。
	if out[3].ToolCallID != "call_1" {
		t.Errorf("ToolCallID: want=call_1 got=%q", out[3].ToolCallID)
	}
	if out[3].Name != "run" {
		t.Errorf("Name: want=run got=%q", out[3].Name)
	}
}

// TestConvertMessagesToolCalls 验证 assistant 消息中的 tool_calls 被正确展开。
func TestConvertMessagesToolCalls(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{
			{ID: "tc1", Name: "shell", Arguments: `{"cmd":"ls"}`},
			{ID: "tc2", Name: "read", Arguments: `{"path":"/tmp"}`},
		},
	}
	out := convertMessages([]Message{msg})
	if len(out) != 1 {
		t.Fatalf("消息数量不一致：want=1 got=%d", len(out))
	}
	cm := out[0]
	if len(cm.ToolCalls) != 2 {
		t.Fatalf("ToolCalls 数量不一致：want=2 got=%d", len(cm.ToolCalls))
	}
	// 验证第一个工具调用。
	tc := cm.ToolCalls[0]
	if tc.ID != "tc1" || tc.Function.Name != "shell" || tc.Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("ToolCall[0] 不一致: %+v", tc)
	}
	// type 字段应为 "function"。
	if tc.Type != "function" {
		t.Errorf("ToolCall[0].Type: want=function got=%q", tc.Type)
	}
}

// TestConvertToolsEmpty 验证空 tools 返回 nil（不发送空数组给 API）。
func TestConvertToolsEmpty(t *testing.T) {
	if got := convertTools(nil); got != nil {
		t.Errorf("convertTools(nil) 期望返回 nil，实际 %v", got)
	}
	if got := convertTools([]ToolDef{}); got != nil {
		t.Errorf("convertTools([]) 期望返回 nil，实际 %v", got)
	}
}

// TestConvertToolsSchema 验证工具定义字段（名称、描述、参数 Schema）被完整保留。
func TestConvertToolsSchema(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"city": map[string]interface{}{"type": "string"},
		},
	}
	tools := []ToolDef{
		{Name: "get_weather", Description: "查询天气", Parameters: schema},
	}
	out := convertTools(tools)
	if len(out) != 1 {
		t.Fatalf("工具数量不一致：want=1 got=%d", len(out))
	}
	ct := out[0]
	if ct.Type != "function" {
		t.Errorf("Type: want=function got=%q", ct.Type)
	}
	if ct.Function.Name != "get_weather" {
		t.Errorf("Function.Name: want=get_weather got=%q", ct.Function.Name)
	}
	if ct.Function.Description != "查询天气" {
		t.Errorf("Function.Description: want=查询天气 got=%q", ct.Function.Description)
	}
	if _, ok := ct.Function.Parameters["properties"]; !ok {
		t.Error("Function.Parameters 缺少 properties 字段")
	}
}

// TestIsReasoningModel 验证 OpenAI o 系列推理模型的名称匹配逻辑。
func TestIsReasoningModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"o1-mini", true},
		{"o1-preview", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"gpt-4o", false},
		{"gpt-3.5-turbo", false},
		{"o1", true},
		{"o3", true},
	}
	for _, c := range cases {
		if got := isReasoningModel(c.model); got != c.want {
			t.Errorf("isReasoningModel(%q): want=%v got=%v", c.model, c.want, got)
		}
	}
}

// TestIsReasonerModel 验证 DeepSeek 推理模型的名称匹配逻辑（含大小写）。
func TestIsReasonerModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"deepseek-reasoner", true},
		{"DeepSeek-Reasoner", true}, // 大小写不敏感
		{"deepseek-chat", false},
		{"deepseek-coder", false},
	}
	for _, c := range cases {
		if got := isReasonerModel(c.model); got != c.want {
			t.Errorf("isReasonerModel(%q): want=%v got=%v", c.model, c.want, got)
		}
	}
}

// TestOptionsSerialization 验证生成参数（温度等）被正确写入请求体。
func TestOptionsSerialization(t *testing.T) {
	bodyCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, "[DONE]")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv)
	defer p.Close()

	ch, err := p.Chat(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil,
		WithTemperature(0.5), WithMaxTokens(128), WithTopP(0.9), WithStop("END"))
	if err != nil {
		t.Fatalf("Chat 调用失败: %v", err)
	}
	// 消费完流。
	collectStream(t, ch)

	body := <-bodyCh
	for _, want := range []string{`"temperature":0.5`, `"max_tokens":128`, `"top_p":0.9`, `"stop":["END"]`, `"model":"gpt-test"`} {
		if !strings.Contains(body, want) {
			t.Errorf("请求体缺少 %q，实际请求体: %s", want, body)
		}
	}
}
