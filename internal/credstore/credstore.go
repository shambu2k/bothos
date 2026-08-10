// Package credstore resolves GitHub credentials for the executor. It is the
// only place a token leaves the environment/kms — the executor reads a PAT here
// and hands it to the GitHubWriter, which holds it only for the duration of the
// write. Workers and the agent never see it.
package credstore

import (
	"context"
	"fmt"

	"github.com/shambu2k/bothos/internal/intent"
)

// Env resolves tokens from environment variables (a stand-in for a keyring/
// secrets manager in production). Tokens are gitignored and never logged.
type Env struct {
	lookup func(string) string
}

func NewEnv(lookup func(string) string) *Env { return &Env{lookup: lookup} }

// Resolve returns the PAT for an account+scope. Write scopes prefer the
// per-account GITHUB_WRITE_TOKEN_<ACCOUNT>, falling back to the global
// GITHUB_WRITE_TOKEN; the read-only token is kept separate (GITHUB_READ_TOKEN)
// for the clone/scan path. An account-level token is an optional override, not
// a hard requirement.
func (e *Env) Resolve(ctx context.Context, accountID string, scope intent.TokenScope) (string, error) {
	if scope == intent.TokenReadOnly {
		if v := e.lookup("GITHUB_READ_TOKEN"); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("no GITHUB_READ_TOKEN configured")
	}
	if accountID != "" {
		if v := e.lookup("GITHUB_WRITE_TOKEN_" + accountID); v != "" {
			return v, nil
		}
	}
	if v := e.lookup("GITHUB_WRITE_TOKEN"); v != "" {
		return v, nil
	}
	if accountID != "" {
		return "", fmt.Errorf("no write token configured for account %q (GITHUB_WRITE_TOKEN_%s nor global GITHUB_WRITE_TOKEN)", accountID, accountID)
	}
	return "", fmt.Errorf("no GITHUB_WRITE_TOKEN configured")
}
