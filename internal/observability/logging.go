// 全局 slog.Logger
package observability

import (
	"log/slog"
	"os"
)

func NewLogger() *slog.Logger {
	handler := slog.NewJSONHandler(
		os.Stdout,
		&slog.HandlerOptions{
			Level: slog.LevelInfo,
		},
	)

	return slog.New(handler).With(
		"service", "agentnexus",
	)
}
