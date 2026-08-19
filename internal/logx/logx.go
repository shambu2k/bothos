// Package logx is a tiny helper around log/slog so binaries and hot paths can
// emit JSON logs with a run_id correlation field.
package logx

import (
	"log/slog"
	"os"
)

// New returns a JSON slog logger with a required-level env knob
// (BOTHOS_LOG_LEVEL, default "info").
func New() *slog.Logger {
	lvl := slog.LevelInfo
	if v := os.Getenv("BOTHOS_LOG_LEVEL"); v != "" {
		_ = lvl.UnmarshalText([]byte(v))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// WithRun returns a logger carrying runID as a structured field.
func WithRun(l *slog.Logger, runID string) *slog.Logger {
	return l.With("run_id", runID)
}
