package agent

import (
	"testing"

	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// TestDeduplicateLog 验证日志去重：正确处理 checkpoint 尾部与日志头部重叠的情况。
func TestDeduplicateLog(t *testing.T) {
	msg := func(role provider.Role, content string) provider.Message {
		return provider.Message{Role: role, Content: content}
	}

	tests := []struct {
		name       string
		checkpoint []provider.Message
		log        []provider.Message
		want       []provider.Message // 期望去重后的日志（追加到 checkpoint 之后）
	}{
		{
			name:       "空 checkpoint，日志原样返回",
			checkpoint: nil,
			log:        []provider.Message{msg(provider.RoleUser, "a")},
			want:       []provider.Message{msg(provider.RoleUser, "a")},
		},
		{
			name:       "空日志，返回空",
			checkpoint: []provider.Message{msg(provider.RoleUser, "a")},
			log:        nil,
			want:       nil,
		},
		{
			name: "无重叠，日志完整保留",
			checkpoint: []provider.Message{
				msg(provider.RoleUser, "u1"),
				msg(provider.RoleAssistant, "a1"),
			},
			log: []provider.Message{
				msg(provider.RoleUser, "u2"),
				msg(provider.RoleAssistant, "a2"),
			},
			want: []provider.Message{
				msg(provider.RoleUser, "u2"),
				msg(provider.RoleAssistant, "a2"),
			},
		},
		{
			name: "日志头部 1 条与 checkpoint 尾部重叠",
			checkpoint: []provider.Message{
				msg(provider.RoleUser, "u1"),
				msg(provider.RoleAssistant, "a1"),
				msg(provider.RoleUser, "u2"), // 重叠
			},
			log: []provider.Message{
				msg(provider.RoleUser, "u2"), // 与 checkpoint 尾部重叠
				msg(provider.RoleAssistant, "a2"),
			},
			want: []provider.Message{
				msg(provider.RoleAssistant, "a2"),
			},
		},
		{
			name: "日志头部 2 条完全重叠",
			checkpoint: []provider.Message{
				msg(provider.RoleUser, "u1"),
				msg(provider.RoleAssistant, "a1"),
			},
			log: []provider.Message{
				msg(provider.RoleUser, "u1"),
				msg(provider.RoleAssistant, "a1"),
				msg(provider.RoleUser, "u2"),
			},
			want: []provider.Message{
				msg(provider.RoleUser, "u2"),
			},
		},
		{
			name: "日志与 checkpoint 完全重叠，返回空",
			checkpoint: []provider.Message{
				msg(provider.RoleUser, "u1"),
				msg(provider.RoleAssistant, "a1"),
			},
			log: []provider.Message{
				msg(provider.RoleUser, "u1"),
				msg(provider.RoleAssistant, "a1"),
			},
			want: []provider.Message{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deduplicateLog(tc.checkpoint, tc.log)
			if len(got) != len(tc.want) {
				t.Fatalf("去重后长度不一致：want=%d got=%d", len(tc.want), len(got))
			}
			for i := range got {
				if got[i].Content != tc.want[i].Content || got[i].Role != tc.want[i].Role {
					t.Errorf("第 %d 条不一致：want=%+v got=%+v", i, tc.want[i], got[i])
				}
			}
		})
	}
}

// TestMessageEqual 验证单条消息相等判断。
func TestMessageEqual(t *testing.T) {
	base := provider.Message{
		Role:    provider.RoleUser,
		Content: "hello",
		ToolCalls: []provider.ToolCall{
			{ID: "tc1", Name: "run", Arguments: `{"cmd":"ls"}`},
		},
	}

	// 完全相同。
	if !messageEqual(base, base) {
		t.Error("相同消息应相等")
	}

	// 内容不同。
	diff := base
	diff.Content = "world"
	if messageEqual(base, diff) {
		t.Error("内容不同应不相等")
	}

	// ToolCall 参数不同。
	diff2 := base
	diff2.ToolCalls = []provider.ToolCall{
		{ID: "tc1", Name: "run", Arguments: `{"cmd":"pwd"}`},
	}
	if messageEqual(base, diff2) {
		t.Error("ToolCall 参数不同应不相等")
	}

	// ToolCall 数量不同。
	diff3 := base
	diff3.ToolCalls = nil
	if messageEqual(base, diff3) {
		t.Error("ToolCall 数量不同应不相等")
	}
}
