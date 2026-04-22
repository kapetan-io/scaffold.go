package scaffold

import (
	"log/slog"

	"github.com/kapetan-io/tackle/color"
)

// defaultLogger returns the colorized, timestamp-suppressed *slog.Logger
// that scaffold uses when Options.Log is nil. The handler is always
// colorized regardless of whether stdout is a TTY; users who pipe output
// to a file should provide their own *slog.Logger via Options.Log.
func defaultLogger() *slog.Logger {
	handler := color.NewLog(&color.LogOptions{
		HandlerOptions: slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: color.SuppressAttrs(slog.TimeKey),
		},
	})
	return slog.New(handler)
}
