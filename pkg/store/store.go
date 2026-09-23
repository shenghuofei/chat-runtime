// Package store 负责会话持久化。
//
// 采用 checkpoint（会话结构化快照）+ JSONL 增量日志的组合：
//   - checkpoint（<id>.json）记录当前完整的会话数据（消息历史、系统提示词、
//     token 用量、元数据），用于快速恢复；
//   - 增量日志（<id>.jsonl）以追加方式逐条记录消息事件，提供细粒度审计与
//     断点续写能力。
package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/shenghuofei/chat-runtime/pkg/provider"
)

// SessionData 表示一个会话的完整快照数据。
type SessionData struct {
	// Messages 完整的消息历史。
	Messages []provider.Message `json:"messages"`
	// SystemPrompt 系统提示词（渲染后的最终文本）。
	SystemPrompt string `json:"system_prompt"`
	// TokenUsage 累计 token 用量。
	TokenUsage provider.TokenUsage `json:"token_usage"`
	// Metadata 附加元数据（如所属对话名、模型名等）。
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Store 定义会话持久化的抽象接口。
//
// 上层（Session）只依赖该接口，具体实现可以是文件系统、数据库等。
// 所有方法均接收 context.Context 作为第一参数，以便实现方支持超时与取消。
type Store interface {
	// Save 保存（覆盖）指定会话的快照。
	Save(ctx context.Context, sessionID string, data SessionData) error
	// Load 加载指定会话的快照；不存在时返回错误。
	Load(ctx context.Context, sessionID string) (*SessionData, error)
	// AppendMessage 向指定会话的增量日志追加一条消息。
	AppendMessage(ctx context.Context, sessionID string, msg provider.Message) error
	// LoadLog 读取指定会话的增量日志；不存在时返回空切片且不报错。
	LoadLog(ctx context.Context, sessionID string) ([]provider.Message, error)
	// Delete 删除指定会话的快照与增量日志（幂等）。
	Delete(ctx context.Context, sessionID string) error
}

// FileStore 是基于本地文件系统的 Store 实现。
//
// 每个会话对应两个文件：
//   - <baseDir>/<id>.json  —— checkpoint 快照；
//   - <baseDir>/<id>.jsonl —— 增量日志（每行一条 JSON）。
//
// 使用 sync.Map 按会话 ID 维护独立的 *sync.Mutex，避免所有会话共用单把全局锁，
// 消除多会话并发时的不必要争用。
type FileStore struct {
	baseDir string
	// sessions 存储 sessionID -> *sync.Mutex 的映射，保证每个会话独立加锁。
	sessions sync.Map
}

// 编译期断言：FileStore 实现了 Store 接口。
var _ Store = (*FileStore)(nil)

// NewFileStore 创建一个 FileStore，baseDir 为持久化根目录（不存在则创建）。
func NewFileStore(baseDir string) (*FileStore, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("baseDir 不能为空")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建持久化目录失败：%w", err)
	}
	return &FileStore{baseDir: baseDir}, nil
}

// sessionLock 返回指定会话 ID 对应的锁（不存在则新建），保证并发安全。
func (s *FileStore) sessionLock(sessionID string) *sync.Mutex {
	mu, _ := s.sessions.LoadOrStore(sessionID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// checkpointPath 返回会话快照文件路径。
func (s *FileStore) checkpointPath(sessionID string) string {
	return filepath.Join(s.baseDir, sessionID+".json")
}

// logPath 返回会话增量日志文件路径。
func (s *FileStore) logPath(sessionID string) string {
	return filepath.Join(s.baseDir, sessionID+".jsonl")
}

// Save 将会话快照原子性地写入磁盘（先写临时文件再 rename）。
func (s *FileStore) Save(_ context.Context, sessionID string, data SessionData) error {
	mu := s.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话数据失败：%w", err)
	}

	path := s.checkpointPath(sessionID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("写入临时快照失败：%w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("提交快照失败：%w", err)
	}

	// 快照写入成功：截断增量日志，以新快照为恢复基准，避免重复追加旧消息。
	// 截断失败不影响快照完整性，仅在下次 Save 时重试。
	_ = os.Truncate(s.logPath(sessionID), 0)
	return nil
}

// Load 读取会话快照；文件不存在时返回错误。
func (s *FileStore) Load(_ context.Context, sessionID string) (*SessionData, error) {
	mu := s.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	buf, err := os.ReadFile(s.checkpointPath(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("会话 %q 不存在", sessionID)
		}
		return nil, fmt.Errorf("读取快照失败：%w", err)
	}
	var data SessionData
	if err := json.Unmarshal(buf, &data); err != nil {
		return nil, fmt.Errorf("反序列化快照失败：%w", err)
	}
	return &data, nil
}

// AppendMessage 以追加方式向增量日志写入一条消息（每行一条 JSON）。
func (s *FileStore) AppendMessage(_ context.Context, sessionID string, msg provider.Message) error {
	mu := s.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(s.logPath(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开增量日志失败：%w", err)
	}
	defer f.Close()

	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("序列化日志消息失败：%w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("写入增量日志失败：%w", err)
	}
	return nil
}

// LoadLog 逐行读取增量日志并还原为消息列表；文件不存在时返回空切片。
func (s *FileStore) LoadLog(_ context.Context, sessionID string) ([]provider.Message, error) {
	mu := s.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.Open(s.logPath(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开增量日志失败：%w", err)
	}
	defer f.Close()

	var msgs []provider.Message
	scanner := bufio.NewScanner(f)
	// 放宽单行长度上限，避免长消息（如大段工具输出）被截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg provider.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("解析增量日志行失败：%w", err)
		}
		msgs = append(msgs, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取增量日志失败：%w", err)
	}
	return msgs, nil
}

// Delete 删除会话的快照与增量日志。对不存在的文件保持幂等（不报错）。
// 删除成功后同时清理 sync.Map 中的锁条目，避免长期运行的服务因大量短命会话
// 造成锁表内存泄漏。
func (s *FileStore) Delete(_ context.Context, sessionID string) error {
	mu := s.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	for _, path := range []string{s.checkpointPath(sessionID), s.logPath(sessionID)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除文件 %q 失败：%w", path, err)
		}
	}

	// 文件已删，后续不会再有并发访问，安全地移除锁条目。
	s.sessions.Delete(sessionID)
	return nil
}
