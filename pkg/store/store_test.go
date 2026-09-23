package store

import (
	"context"
	"testing"

	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// TestSaveLoadRoundtrip 验证保存后再加载能还原完整会话数据。
func TestSaveLoadRoundtrip(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}

	want := SessionData{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "你是助手"},
			{Role: provider.RoleUser, Content: "你好"},
			{Role: provider.RoleAssistant, Content: "你好！"},
		},
		SystemPrompt: "你是助手",
		TokenUsage:   provider.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		Metadata:     map[string]any{"chat": "default", "model": "deepseek-chat"},
	}

	ctx := context.Background()

	if err := s.Save(ctx, "sess-1", want); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	got, err := s.Load(ctx, "sess-1")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}

	if got.SystemPrompt != want.SystemPrompt {
		t.Errorf("SystemPrompt 不一致：want=%q got=%q", want.SystemPrompt, got.SystemPrompt)
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("消息数量不一致：want=%d got=%d", len(want.Messages), len(got.Messages))
	}
	if got.Messages[1].Content != "你好" {
		t.Errorf("消息内容不一致：%+v", got.Messages[1])
	}
	if got.TokenUsage.TotalTokens != 15 {
		t.Errorf("TokenUsage 不一致：%+v", got.TokenUsage)
	}
	if got.Metadata["chat"] != "default" {
		t.Errorf("Metadata 不一致：%+v", got.Metadata)
	}
}

// TestAppendMessage 验证增量日志的追加与回放。
func TestAppendMessage(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}

	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "第一条"},
		{Role: provider.RoleAssistant, Content: "第二条"},
		{Role: provider.RoleTool, ToolCallID: "c1", Name: "ls", Content: "第三条"},
	}
	ctx := context.Background()
	for _, m := range msgs {
		if err := s.AppendMessage(ctx, "sess-2", m); err != nil {
			t.Fatalf("追加消息失败：%v", err)
		}
	}

	got, err := s.LoadLog(ctx, "sess-2")
	if err != nil {
		t.Fatalf("读取增量日志失败：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 条日志，实际 %d 条", len(got))
	}
	if got[0].Content != "第一条" || got[2].ToolCallID != "c1" {
		t.Errorf("日志内容不一致：%+v", got)
	}
}

// TestLoadLogEmpty 验证不存在的日志返回空而非错误。
func TestLoadLogEmpty(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}
	got, err := s.LoadLog(context.Background(), "not-exist")
	if err != nil {
		t.Errorf("不存在的日志不应报错：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("期望空日志，实际 %d 条", len(got))
	}
}

// TestDelete 验证删除会同时清理 checkpoint 与增量日志，且具备幂等性。
func TestDelete(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}

	ctx := context.Background()

	if err := s.Save(ctx, "sess-3", SessionData{SystemPrompt: "x"}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if err := s.AppendMessage(ctx, "sess-3", provider.Message{Content: "y"}); err != nil {
		t.Fatalf("追加失败：%v", err)
	}

	if err := s.Delete(ctx, "sess-3"); err != nil {
		t.Fatalf("删除失败：%v", err)
	}

	// 删除后再加载应报错（不存在）。
	if _, err := s.Load(ctx, "sess-3"); err == nil {
		t.Error("删除后加载应报错")
	}

	// 幂等：再次删除不应报错。
	if err := s.Delete(ctx, "sess-3"); err != nil {
		t.Errorf("重复删除应幂等，不应报错：%v", err)
	}
}

// TestLoadNotExist 验证加载不存在的会话返回错误。
func TestLoadNotExist(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 FileStore 失败：%v", err)
	}
	if _, err := s.Load(context.Background(), "ghost"); err == nil {
		t.Error("加载不存在的会话应报错")
	}
}
