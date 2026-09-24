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
	"sort"
	"strings"
	"sync"
	"time"

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
	// Release 释放指定会话的内存资源（写入器、锁条目），但保留磁盘文件。
	// Web 模式会话结束时应调用此方法，防止 sync.Map 条目与文件句柄泄漏。
	Release(ctx context.Context, sessionID string)
	// ListSessions 扫描持久化目录，返回所有会话的摘要信息。
	ListSessions(ctx context.Context) ([]SessionInfo, error)
}

// SessionInfo 是会话列表中每条的摘要信息。
type SessionInfo struct {
	// ID 会话唯一标识（从文件名提取）。
	ID string `json:"id"`
	// ChatName 会话所属的对话预设名（从 Metadata["chat"] 读取）。
	ChatName string `json:"chat"`
	// Summary 第一条 user 消息的前 80 字符，用于侧边栏预览。
	Summary string `json:"summary"`
	// UpdatedAt checkpoint 文件的最后修改时间。
	UpdatedAt time.Time `json:"updated_at"`
	// MessageCount 消息总数。
	MessageCount int `json:"message_count"`
}

// logWriter 封装一个带缓冲的增量日志写入器。
//
// 使用 bufio.Writer 减少对底层文件的 syscall 次数：在高频 Tool Calling
// 场景下（一轮可能产生多次工具调用），批量写入比每次 AppendMessage 都
// open/write/close 要高效得多。
type logWriter struct {
	file *os.File
	buf  *bufio.Writer
}

// newLogWriter 创建一个带缓冲的日志写入器。
func newLogWriter(path string) (*logWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &logWriter{
		file: f,
		buf:  bufio.NewWriterSize(f, 8*1024), // 8KB 缓冲
	}, nil
}

// write 将一条消息写入缓冲区（每行一条 JSON）。
//
// 设计选择：这里刻意不做 flush。数据仅进入 bufio.Writer 的内存缓冲区，
// 待缓冲区写满（8KB）时才由 bufio 自动触发一次底层写 syscall。
// 在高频 Tool Calling 场景下（一轮可能产生多条工具调用消息），
// 多条消息会在缓冲区累积后合并落盘，显著减少 syscall 次数。
//
// 缓冲区中的数据会在以下时机确保落盘：
//   - LoadLog 开头（读取前先 flush）；
//   - Save 成功后 truncate（内部先 flush 再截断）；
//   - Delete / close 关闭写入器时（先 flush 再关闭）。
//
// 权衡：进程崩溃时可能丢失缓冲区中尚未落盘的少量日志。但这是可接受的——
// checkpoint 快照 + 增量日志的双重保障设计本身即容忍少量日志丢失，
// 恢复时以 checkpoint 为基准，缺失的尾部日志不会破坏会话一致性。
func (lw *logWriter) write(msg provider.Message) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("序列化日志消息失败：%w", err)
	}
	// 分两次写入避免 append(line, '\n') 产生额外堆分配：
	// bufio.Writer 会将两次写入合并在缓冲区内，不额外触发 syscall。
	if _, err := lw.buf.Write(line); err != nil {
		return fmt.Errorf("写入增量日志失败：%w", err)
	}
	if err := lw.buf.WriteByte('\n'); err != nil {
		return fmt.Errorf("写入增量日志换行失败：%w", err)
	}
	return nil
}

// close 关闭写入器，先 flush 再关闭文件。
func (lw *logWriter) close() error {
	if err := lw.buf.Flush(); err != nil {
		lw.file.Close()
		return err
	}
	return lw.file.Close()
}

// truncate 截断日志文件（Save 成功后调用），flush 缓冲并将文件截断为 0。
func (lw *logWriter) truncate() error {
	// 先 flush 缓冲区中可能残留的数据。
	_ = lw.buf.Flush()
	// 截断文件。
	if err := lw.file.Truncate(0); err != nil {
		return err
	}
	// 将写入位置移回文件开头。
	_, err := lw.file.Seek(0, 0)
	if err != nil {
		return err
	}
	// 重置 bufio.Writer 的内部状态。
	lw.buf.Reset(lw.file)
	return nil
}

// sessionState 封装单个会话的并发锁与日志写入器。
//
// 使用 sync.RWMutex：Load 只读取 checkpoint 文件，不涉及 writer，
// 可与 AppendMessage 并发执行；其余变更操作仍持写锁。
type sessionState struct {
	mu     sync.RWMutex
	writer *logWriter // 延迟初始化，首次 AppendMessage 时创建
}

// FileStore 是基于本地文件系统的 Store 实现。
//
// 每个会话对应两个文件：
//   - <baseDir>/<id>.json  —— checkpoint 快照；
//   - <baseDir>/<id>.jsonl —— 增量日志（每行一条 JSON）。
//
// 使用 sync.Map 按会话 ID 维护独立的 sessionState（含锁与写入器），
// 避免所有会话共用单把全局锁，消除多会话并发时的不必要争用。
type FileStore struct {
	baseDir string
	// sessions 存储 sessionID -> *sessionState 的映射。
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

// getState 返回指定会话 ID 对应的状态（不存在则新建）。
func (s *FileStore) getState(sessionID string) *sessionState {
	state, _ := s.sessions.LoadOrStore(sessionID, &sessionState{})
	return state.(*sessionState)
}

// checkpointPath 返回会话快照文件路径。
func (s *FileStore) checkpointPath(sessionID string) string {
	return filepath.Join(s.baseDir, sessionID+".json")
}

// logPath 返回会话增量日志文件路径。
func (s *FileStore) logPath(sessionID string) string {
	return filepath.Join(s.baseDir, sessionID+".jsonl")
}

// writeFsync 写入数据到指定路径并在关闭前调用 fsync，保证数据在 rename 前落盘。
// 若写入或 sync 失败，自动删除已创建的文件，避免留下残损文件。
func writeFsync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// Save 将会话快照原子性地写入磁盘（先写临时文件 + fsync，再 rename）。
//
// fsync 保证临时文件数据在 rename 前已落盘：若进程在 rename 后崩溃，
// 目标文件必然包含完整内容；若在 rename 前崩溃，临时文件可安全忽略。
func (s *FileStore) Save(_ context.Context, sessionID string, data SessionData) error {
	state := s.getState(sessionID)
	state.mu.Lock()
	defer state.mu.Unlock()

	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话数据失败：%w", err)
	}

	path := s.checkpointPath(sessionID)
	tmp := path + ".tmp"
	if err := writeFsync(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("写入临时快照失败：%w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("提交快照失败：%w", err)
	}

	// 快照写入成功：截断增量日志，以新快照为恢复基准。
	if state.writer != nil {
		_ = state.writer.truncate()
	} else {
		_ = os.Truncate(s.logPath(sessionID), 0)
	}
	return nil
}

// Load 读取会话快照；文件不存在时返回错误。
//
// 使用读锁：Load 仅读取 checkpoint 文件，不修改 state，可与 AppendMessage 并发执行。
func (s *FileStore) Load(_ context.Context, sessionID string) (*SessionData, error) {
	state := s.getState(sessionID)
	state.mu.RLock()
	defer state.mu.RUnlock()

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

// AppendMessage 通过带缓冲的写入器向增量日志追加一条消息。
//
// 优化：维持文件句柄不关闭，使用 bufio.Writer 批量写入，
// 相比原实现每次 open/write/close 减少 2/3 的 syscall。
func (s *FileStore) AppendMessage(_ context.Context, sessionID string, msg provider.Message) error {
	state := s.getState(sessionID)
	state.mu.Lock()
	defer state.mu.Unlock()

	// 延迟初始化写入器。
	if state.writer == nil {
		w, err := newLogWriter(s.logPath(sessionID))
		if err != nil {
			return fmt.Errorf("打开增量日志失败：%w", err)
		}
		state.writer = w
	}

	return state.writer.write(msg)
}

// LoadLog 逐行读取增量日志并还原为消息列表；文件不存在时返回空切片。
func (s *FileStore) LoadLog(_ context.Context, sessionID string) ([]provider.Message, error) {
	state := s.getState(sessionID)
	state.mu.Lock()
	defer state.mu.Unlock()

	// 如果有活跃写入器，先 flush 确保所有数据已落盘。
	if state.writer != nil {
		_ = state.writer.buf.Flush()
	}

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

// Release 释放指定会话的写入器与 sync.Map 条目，但不删除磁盘文件。
//
// Web 模式下每个连接使用随机 sessionID，会话结束时需调用此方法释放资源
// （文件句柄与 map 条目），否则长运行服务会因大量已结束会话累积而泄漏。
// 与 Delete 不同，Release 保留磁盘上的 checkpoint 和 log 文件。
func (s *FileStore) Release(_ context.Context, sessionID string) {
	state := s.getState(sessionID)
	state.mu.Lock()
	if state.writer != nil {
		_ = state.writer.close()
		state.writer = nil
	}
	state.mu.Unlock()
	s.sessions.Delete(sessionID)
}

// Delete 删除会话的快照与增量日志。对不存在的文件保持幂等（不报错）。
// 删除成功后同时关闭写入器并清理 sync.Map 中的状态条目，避免长期运行的服务
// 因大量短命会话造成内存泄漏。
func (s *FileStore) Delete(_ context.Context, sessionID string) error {
	state := s.getState(sessionID)
	state.mu.Lock()
	defer state.mu.Unlock()

	// 关闭活跃写入器。
	if state.writer != nil {
		_ = state.writer.close()
		state.writer = nil
	}

	for _, path := range []string{s.checkpointPath(sessionID), s.logPath(sessionID)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除文件 %q 失败：%w", path, err)
		}
	}

	// 文件已删，后续不会再有并发访问，安全地移除状态条目。
	s.sessions.Delete(sessionID)
	return nil
}

// ListSessions 扫描 baseDir 下所有 .json checkpoint 文件，
// 返回每个会话的摘要信息（ID、chat 名、首条 user 消息摘要、修改时间、消息数）。
//
// 只读操作，不获取 sessionState 锁（checkpoint 通过原子 rename 写入，
// 读取不会看到半写内容）。按 UpdatedAt 降序排列（最近活跃的排前面）。
func (s *FileStore) ListSessions(_ context.Context) ([]SessionInfo, error) {
	pattern := filepath.Join(s.baseDir, "*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("扫描会话目录失败：%w", err)
	}

	infos := make([]SessionInfo, 0, len(files))
	for _, path := range files {
		// 从文件名提取 session ID（去掉 .json 后缀）。
		base := filepath.Base(path)
		sessionID := strings.TrimSuffix(base, ".json")

		// 读取文件修改时间。
		fi, err := os.Stat(path)
		if err != nil {
			continue // 文件可能刚被删除，跳过
		}

		// 读取 checkpoint 内容提取摘要。
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data SessionData
		if err := json.Unmarshal(raw, &data); err != nil {
			continue // 损坏的 checkpoint，跳过
		}

		info := SessionInfo{
			ID:           sessionID,
			UpdatedAt:    fi.ModTime(),
			MessageCount: len(data.Messages),
		}

		// 从 Metadata 提取 chat 名。
		if chat, ok := data.Metadata["chat"]; ok {
			if chatStr, ok := chat.(string); ok {
				info.ChatName = chatStr
			}
		}

		// 取第一条 user 消息的前 80 字符作为摘要。
		for _, msg := range data.Messages {
			if msg.Role == provider.RoleUser && msg.Content != "" {
				r := []rune(msg.Content)
				if len(r) > 80 {
					info.Summary = string(r[:80]) + "…"
				} else {
					info.Summary = msg.Content
				}
				break
			}
		}

		infos = append(infos, info)
	}

	// 按最后修改时间降序排列。
	sortSessionInfos(infos)
	return infos, nil
}

// sortSessionInfos 按 UpdatedAt 降序排列（最近的排前面）。
func sortSessionInfos(infos []SessionInfo) {
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
	})
}
