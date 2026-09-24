// Package provider — 各 Provider 共享的辅助函数与常量。
package provider

import (
	"context"
	"io"
	"math"
	"net/http"
	"time"
)

// 各 Provider 共享的 HTTP 与重试常量。
const (
	// maxRetries 针对可重试错误（网络错误 / 5xx）的最大重试次数。
	maxRetries = 3
	// baseRetryDelay 指数退避的基础延迟。
	baseRetryDelay = 500 * time.Millisecond
	// responseHeaderTimeout 建连并收到响应首字节（响应头）的超时时间。
	// 用于替代已弃用的整体请求超时：流式对话的整体耗时不应被限制，
	// 但建连/首包阶段仍需超时保护，避免连接 hang 住。
	responseHeaderTimeout = 30 * time.Second
)

// requestTimeout 已弃用：整体请求超时会截断长时间的流式响应，
// 现改为 Timeout: 0（不限制整体时间，由 ctx 控制），
// 并通过 Transport.ResponseHeaderTimeout 保证建连/首包超时。
// const requestTimeout = 5 * time.Minute

// newHTTPClient 创建一个配置了合理连接池参数的 HTTP 客户端。
//
// 所有 Provider 共用此函数，避免每个 Provider 使用默认的空 Transport
// 导致连接池参数不优。
//
// Timeout 设为 0（不限制整体请求时间）：流式对话可能持续很久，
// 整体超时会错误地截断长响应。请求的取消与超时应由传入的 ctx 控制。
// 为避免连接在建连/首包阶段 hang 住，Transport 设置 ResponseHeaderTimeout。
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
		},
	}
}

// retrySleep 在重试前按指数退避休眠，返回 false 表示 context 已取消。
//
// 使用 time.NewTimer 替代 time.After：time.After 返回的 channel 在计时器触发前
// 无法被 GC 回收；若 ctx 先取消，泄漏的 timer 会在 delay 到期前一直占用资源。
// NewTimer + defer Stop() 确保 ctx 取消后计时器立即释放。
func retrySleep(ctx context.Context, attempt int) bool {
	delay := time.Duration(float64(baseRetryDelay) * math.Pow(2, float64(attempt-1)))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// truncateBody 截断响应体用于错误日志，避免日志过长。
func truncateBody(body []byte) string {
	if len(body) == 0 {
		return "(空响应体)"
	}
	const maxLen = 512
	if len(body) > maxLen {
		return string(body[:maxLen]) + "..."
	}
	return string(body)
}

// readErrorBody 读取错误响应体，忽略读取失败（返回空字节数组）。
// 用于在 doWithRetry 中读取非 2xx 响应体时处理 io.ReadAll 可能的错误。
//
// 使用 io.LimitReader 限制最大读取量，并通过 io.ReadAll 循环读取，
// 避免单次 Read 因 TCP 分片只返回部分数据而丢失错误信息。
func readErrorBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	// 限制错误响应体最大读取量，避免异常大响应耗尽内存。
	const maxBodySize = 64 * 1024
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	return buf
}
