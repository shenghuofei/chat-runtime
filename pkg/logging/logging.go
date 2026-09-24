// Package logging 提供基于 log/slog 的结构化日志基础设施。
//
// 全局使用 Init 初始化后，各模块通过 slog.Default() 即可获取 logger；
// 或者调用 logging.With(...) 创建带上下文字段的子 logger。
//
// 支持两种输出格式：
//   - text（默认）：适合本地开发与 CLI 模式，人类可读；
//   - json：适合生产部署、日志聚合系统（如 ELK / Loki）。
//
// 支持日志级别控制：debug / info / warn / error。
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Init 初始化全局 slog 默认 logger。
//
// 参数：
//   - level: 日志级别，"debug" / "info" / "warn" / "error"，不区分大小写。
//   - format: 输出格式，"json" 使用 JSON 行格式，其它值使用 text 格式。
func Init(level, format string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if strings.ToLower(format) == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// With 返回一个带附加字段的 slog.Logger（基于当前默认 logger）。
func With(args ...any) *slog.Logger {
	return slog.Default().With(args...)
}
