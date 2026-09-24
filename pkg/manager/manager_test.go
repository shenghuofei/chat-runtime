package manager

import (
	"strings"
	"testing"

	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// TestAddAndGetMessages 验证消息的追加与读取：system + 多轮历史。
func TestAddAndGetMessages(t *testing.T) {
	m := New(ManagerConfig{Mode: ModeTruncate, MaxRounds: 10})
	m.SetSystemPrompt("你是助手")

	m.AddUserMessage(provider.Message{Content: "你好"})
	m.AddAssistantMessage(provider.Message{Content: "你好，有什么可以帮你？"})
	m.AddUserMessage(provider.Message{Content: "今天天气如何"})
	m.AddAssistantMessage(provider.Message{Content: "我无法获取实时天气"})

	msgs := m.GetMessages()

	// 期望：system + 2 轮 * (user + assistant) = 1 + 4 = 5 条。
	if len(msgs) != 5 {
		t.Fatalf("期望 5 条消息，实际 %d 条", len(msgs))
	}
	if msgs[0].Role != provider.RoleSystem || msgs[0].Content != "你是助手" {
		t.Errorf("首条应为 system 提示词，实际 %+v", msgs[0])
	}
	if msgs[1].Role != provider.RoleUser || msgs[1].Content != "你好" {
		t.Errorf("第二条应为首个 user 消息，实际 %+v", msgs[1])
	}
	if msgs[4].Role != provider.RoleAssistant {
		t.Errorf("最后一条应为 assistant 响应，实际 %+v", msgs[4])
	}
	if m.RoundCount() != 2 {
		t.Errorf("期望 2 轮，实际 %d 轮", m.RoundCount())
	}
}

// TestTruncateOverflow 验证 truncate 模式：超过 MaxRounds 时丢弃最早轮次。
func TestTruncateOverflow(t *testing.T) {
	m := New(ManagerConfig{Mode: ModeTruncate, MaxRounds: 3})
	m.SetSystemPrompt("sys")

	// 追加 5 轮对话。
	for i := 0; i < 5; i++ {
		m.AddUserMessage(provider.Message{Content: "u" + string(rune('0'+i))})
		m.AddAssistantMessage(provider.Message{Content: "a" + string(rune('0'+i))})
	}

	msgs := m.GetMessages()

	// 仅保留最近 3 轮。
	if m.RoundCount() != 3 {
		t.Fatalf("truncate 后期望 3 轮，实际 %d 轮", m.RoundCount())
	}
	// system + 3 轮 * 2 = 7 条。
	if len(msgs) != 7 {
		t.Fatalf("期望 7 条消息，实际 %d 条", len(msgs))
	}
	// 最早保留的 user 应为 u2（u0、u1 被丢弃）。
	if msgs[1].Content != "u2" {
		t.Errorf("期望最早保留 u2，实际 %s", msgs[1].Content)
	}
}

// TestWindowOverflow 验证 window 模式：token 用量超过阈值时丢弃早期轮次。
func TestWindowOverflow(t *testing.T) {
	// MaxTokens=1000，CompressRatio=0.8，阈值=800。
	m := New(ManagerConfig{Mode: ModeWindow, MaxTokens: 100, CompressRatio: 0.8})

	for i := 0; i < 10; i++ {
		m.AddUserMessage(provider.Message{Content: "这是一段比较长的用户问题内容，用来让估算 token 数超过阈值触发 window 裁剪"})
		m.AddAssistantMessage(provider.Message{Content: "这是一段比较长的助手回答内容，用来让估算 token 数超过阈值触发 window 裁剪"})
	}

	before := m.RoundCount()
	if before != 10 {
		t.Fatalf("上报前期望 10 轮，实际 %d 轮", before)
	}

	// 上报用量（具体值不再影响裁剪决策，裁剪完全基于 estimateTokens 估算）。
	m.ReportUsage(provider.TokenUsage{TotalTokens: 1000})

	after := m.RoundCount()
	if after >= before {
		t.Errorf("上报超阈值用量后应丢弃早期轮次：before=%d after=%d", before, after)
	}
	if after < 1 {
		t.Errorf("至少应保留 1 轮，实际 %d 轮", after)
	}
}

// TestWindowNoOverflow 验证 window 模式：用量在阈值内时不丢弃。
func TestWindowNoOverflow(t *testing.T) {
	m := New(ManagerConfig{Mode: ModeWindow, MaxTokens: 1000, CompressRatio: 0.8})
	for i := 0; i < 5; i++ {
		m.AddUserMessage(provider.Message{Content: "q"})
		m.AddAssistantMessage(provider.Message{Content: "a"})
	}
	m.ReportUsage(provider.TokenUsage{TotalTokens: 500}) // 低于阈值 800
	if m.RoundCount() != 5 {
		t.Errorf("用量在阈值内不应丢弃，期望 5 轮，实际 %d 轮", m.RoundCount())
	}
}

// TestCompressOverflow 验证 compress 模式：超过 MaxRounds 时用摘要替换早期历史。
func TestCompressOverflow(t *testing.T) {
	called := false
	m := New(ManagerConfig{
		Mode:      ModeCompress,
		MaxRounds: 3,
		Summarizer: func(messages []provider.Message) (string, error) {
			called = true
			return "早期对话摘要内容", nil
		},
	})

	for i := 0; i < 5; i++ {
		m.AddUserMessage(provider.Message{Content: "u"})
		m.AddAssistantMessage(provider.Message{Content: "a"})
	}

	// compress 模式下，追加消息仅置位「待压缩」标记，真正的摘要（网络 IO）
	// 由 CompressIfNeeded 在锁外执行，避免持写锁做网络请求。
	m.CompressIfNeeded()

	msgs := m.GetMessages()
	if !called {
		t.Fatal("超过 MaxRounds 时应调用 Summarizer")
	}
	if m.RoundCount() != 3 {
		t.Errorf("压缩后期望保留 3 轮，实际 %d 轮", m.RoundCount())
	}
	// 应存在一条包含摘要内容的消息。
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "早期对话摘要内容") {
			found = true
			break
		}
	}
	if !found {
		t.Error("消息序列中应包含摘要内容")
	}
}

// TestNormalizeToolMessages 验证工具结果按 tool_calls 顺序重排。
func TestNormalizeToolMessages(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "查两件事"},
		{
			Role: provider.RoleAssistant,
			ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "a"},
				{ID: "call_2", Name: "b"},
			},
		},
		// 结果乱序返回：call_2 在前。
		{Role: provider.RoleTool, ToolCallID: "call_2", Content: "结果2"},
		{Role: provider.RoleTool, ToolCallID: "call_1", Content: "结果1"},
	}

	out := NormalizeToolMessages(msgs)

	// 标准化后，call_1 的结果应排在 call_2 之前，与 tool_calls 顺序一致。
	if out[2].ToolCallID != "call_1" {
		t.Errorf("期望首个工具结果为 call_1，实际 %s", out[2].ToolCallID)
	}
	if out[3].ToolCallID != "call_2" {
		t.Errorf("期望第二个工具结果为 call_2，实际 %s", out[3].ToolCallID)
	}
}

// TestNormalizeToolMessagesOrphan 验证孤立工具结果被保留。
func TestNormalizeToolMessagesOrphan(t *testing.T) {
	msgs := []provider.Message{
		{
			Role:      provider.RoleAssistant,
			ToolCalls: []provider.ToolCall{{ID: "call_1"}},
		},
		{Role: provider.RoleTool, ToolCallID: "call_1", Content: "r1"},
		{Role: provider.RoleTool, ToolCallID: "orphan", Content: "游离结果"},
	}
	out := NormalizeToolMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("孤立结果不应被丢弃，期望 3 条，实际 %d 条", len(out))
	}
}

// TestValidateRound 验证轮次配对校验。
func TestValidateRound(t *testing.T) {
	// 正常配对。
	valid := []provider.Message{
		{Role: provider.RoleUser, Content: "q"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1"}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: "r"},
	}
	if err := ValidateRound(valid); err != nil {
		t.Errorf("合法配对不应报错：%v", err)
	}

	// 缺少工具结果。
	missing := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1"}}},
	}
	if err := ValidateRound(missing); err == nil {
		t.Error("缺少工具结果应报错")
	}

	// 游离工具结果。
	orphan := []provider.Message{
		{Role: provider.RoleUser, Content: "q"},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: "r"},
	}
	if err := ValidateRound(orphan); err == nil {
		t.Error("游离工具结果应报错")
	}

	// tool_call_id 不匹配。
	mismatch := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1"}}},
		{Role: provider.RoleTool, ToolCallID: "c2", Content: "r"},
	}
	if err := ValidateRound(mismatch); err == nil {
		t.Error("tool_call_id 不匹配应报错")
	}
}

// TestAddToolResultRound 验证工具结果被正确归属到当前轮次。
func TestAddToolResultRound(t *testing.T) {
	m := New(ManagerConfig{Mode: ModeTruncate, MaxRounds: 10})
	m.AddUserMessage(provider.Message{Content: "执行命令"})
	m.AddAssistantMessage(provider.Message{
		ToolCalls: []provider.ToolCall{{ID: "c1", Name: "ls"}},
	})
	m.AddToolResult("c1", "ls", "file1\nfile2")

	msgs := m.GetMessages()
	// user + assistant + tool = 3 条。
	if len(msgs) != 3 {
		t.Fatalf("期望 3 条消息，实际 %d 条", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleTool || last.ToolCallID != "c1" || last.Name != "ls" {
		t.Errorf("最后一条应为 c1 的工具结果，实际 %+v", last)
	}
}
