package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

func TestNewWritesJSONToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	l := New()
	l.Info("hello", "answer", 42)
	_ = w.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}
	if got["msg"] != "hello" || got["answer"] != float64(42) || got["level"] != "INFO" {
		t.Fatalf("unexpected record: %v", got)
	}
}

// captureHandler records WithAttrs attributes so tests can assert what WithRun
// attaches without depending on a console handler.
type captureHandler struct{ attrs []slog.Attr }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h *captureHandler) Handle(context.Context, slog.Record) error { return nil }
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(h.attrs, attrs...)
	return h
}
func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func TestWithRunAddsRunID(t *testing.T) {
	h := &captureHandler{}
	WithRun(slog.New(h), "run-42").Info("x")

	for _, a := range h.attrs {
		if a.Key == "run_id" && a.Value.String() == "run-42" {
			return
		}
	}
	t.Fatalf("run_id attr missing: %v", h.attrs)
}
