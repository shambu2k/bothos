package runtime

import (
	"testing"
	"time"
)

func TestAgentWall(t *testing.T) {
	if d := time.Duration(AgentWallSeconds) * time.Second; d != 40*time.Minute {
		t.Fatalf("AgentWallSeconds = %d (%v), want 40m", AgentWallSeconds, d)
	}
}
