// Package provider — 各 Provider 共享的辅助函数。
package provider

import (
	"context"
	"math"
	"time"
)

// retrySleep 在重试前按指数退避休眠，返回 false 表示 context 已取消。
func retrySleep(ctx context.Context, attempt int) bool {
	delay := time.Duration(float64(baseRetryDelay) * math.Pow(2, float64(attempt-1)))
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
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
