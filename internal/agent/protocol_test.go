package agent

import (
	"errors"
	"testing"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/runtime"
)

func TestParseEvent(t *testing.T) {
	e, err := ParseEvent([]byte(`{"type":"intent","intent":{"run_id":"r1","kind":"open_pr","payload":{}}}`))
	if err != nil {
		t.Fatalf("parse intent: %v", err)
	}
	if e.Type != EventIntent || e.Intent == nil || e.Intent.RunID != "r1" {
		t.Fatalf("unexpected intent event: %+v", e)
	}
	if e.Intent.Kind != intent.Kind("open_pr") {
		t.Fatalf("kind: %q", e.Intent.Kind)
	}
}

func TestParseEventTool(t *testing.T) {
	e, err := ParseEvent([]byte(`{"type":"tool","tool":"web_search","msg":"searched"}`))
	if err != nil {
		t.Fatalf("parse tool: %v", err)
	}
	if e.Type != EventTool || e.Tool != "web_search" || e.Msg != "searched" {
		t.Fatalf("unexpected: %+v", e)
	}
}

func TestParseEventMalformed(t *testing.T) {
	if _, err := ParseEvent([]byte(`not json`)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
}

var _ = runtime.UpgradeTask{} // enforce the dep compiles in tests too
