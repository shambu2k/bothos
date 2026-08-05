// Package agent defines the language-agnostic sidecar protocol between the Go
// orchestrator and an agent runtime adapter (the first is PI Agent). Keeping
// this contract in a separate package is what lets a future adapter (OpenHands,
// Claude, ...) plug in behind the runtime.AgentRuntime seam.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/runtime"
)

// ErrMalformed marks an unparseable adapter event line.
var ErrMalformed = errors.New("malformed adapter event")

// Request is the one JSON line the Go orchestrator writes to the adapter's
// stdin. It carries the task and limits only — never a grant, token, or repo.
// Targeting is resolved later by the executor.
type Request struct {
	RunID    string              `json:"run_id"`
	Task     runtime.UpgradeTask `json:"task"`
	Worktree string              `json:"worktree"`
	Limits   runtime.Limits      `json:"limits"`
}

// EventType enumerates the adapter->Go event kinds.
type EventType string

const (
	EventLog    EventType = "log"
	EventTool   EventType = "tool"
	EventIntent EventType = "intent"
	EventError  EventType = "error"
)

// Event is one JSON-lines line emitted by the adapter.
type Event struct {
	Type   EventType        `json:"type"`
	Tool   string           `json:"tool,omitempty"`
	Intent *intent.Envelope `json:"intent,omitempty"`
	Msg    string           `json:"msg,omitempty"`
}

// ParseEvent decodes a single adapter line.
func ParseEvent(line []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return Event{}, fmt.Errorf("%w: %s", ErrMalformed, err)
	}
	return e, nil
}
