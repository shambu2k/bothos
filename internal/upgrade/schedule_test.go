package upgrade

import (
	"testing"
	"time"

	"github.com/shambu2k/bothos/internal/intent"
)

func TestGrantForUpgrade(t *testing.T) {
	g := GrantForUpgrade("r1", intent.Repo{Owner: "acme", Name: "repo", AccountID: "acme"}, "main")

	if g.Scope.Kind != intent.ScopeScheduled || g.Scope.BaseRef != "main" {
		t.Fatalf("scope: %+v", g.Scope)
	}
	if len(g.AllowedKinds) != 1 || g.AllowedKinds[0] != "open_pr" {
		t.Fatalf("allowed kinds: %v", g.AllowedKinds)
	}
	if g.TokenScope != intent.TokenContentsWrite {
		t.Fatalf("token scope: %q", g.TokenScope)
	}
	if g.Repo.AccountID != "acme" {
		t.Fatalf("account: %q", g.Repo.AccountID)
	}
	if !g.ExpiresAt.After(time.Now()) {
		t.Fatal("grant should be unexpired at issue")
	}
	if len(g.DeniedPaths) == 0 {
		t.Fatal("grant should deny secret paths by default")
	}
}
