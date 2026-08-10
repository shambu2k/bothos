package intent

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testNow is a fixed reference instant so expiry tests are deterministic.
var testNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// testGrant builds a default valid Grant. Overrides mutate fields; pass
// pointers-to-fields via closures to break a specific assumption.
func testGrant(overrides ...func(*Grant)) Grant {
	g := Grant{
		RunID:        "run-1",
		Repo:         Repo{Owner: "shambu2k", Name: "repo", AccountID: "acct-1"},
		Scope:        Scope{Kind: ScopeIssue, Number: 5, BaseRef: "main", HeadSHA: "abc123"},
		AllowedKinds: []Kind{KindOpenPR, KindPostComment},
		TokenScope:   TokenContentsWrite,
		Limits:       DefaultLimits(),
		IssuedAt:     testNow.Add(-time.Minute),
		ExpiresAt:    testNow.Add(time.Hour),
	}
	for _, f := range overrides {
		f(&g)
	}
	return g
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func openPREnvelope(t *testing.T, g Grant) Envelope {
	t.Helper()
	return Envelope{
		SchemaVersion: int(SchemaVersion),
		RunID:         g.RunID,
		Kind:          KindOpenPR,
		Payload: mustJSON(t, OpenPR{
			Title: "Bump acme to v1.1",
			Body:  "Updates acme dep",
		}),
	}
}

func assertErr(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// ---------- Validate: structural (step 1) ----------

func TestValidateWrongSchemaVersion(t *testing.T) {
	env := openPREnvelope(t, testGrant())
	env.SchemaVersion = 99
	_, err := Validate(env, testGrant(), testNow)
	assertErr(t, err, ErrSchemaVersion)
}

func TestValidateRunIDMismatch(t *testing.T) {
	env := openPREnvelope(t, testGrant())
	env.RunID = "some-other-run"
	_, err := Validate(env, testGrant(), testNow)
	assertErr(t, err, ErrScopeMismatch)
}

func TestValidateUnknownKind(t *testing.T) {
	g := testGrant()
	env := openPREnvelope(t, g)
	env.Kind = Kind("nuke_the_repo")
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrUnknownKind)
}

func TestValidateUnknownPayloadFieldRejected(t *testing.T) {
	g := testGrant()
	env := Envelope{
		SchemaVersion: int(SchemaVersion),
		RunID:         g.RunID,
		Kind:          KindPostComment,
		// sneaky unknown field: drift should be loud, not ignored
		Payload: json.RawMessage(`{"body":"hi","target_branch":"main"}`),
	}
	g.AllowedKinds = []Kind{KindPostComment}
	g.TokenScope = TokenIssuesWrite
	g.Scope = Scope{Kind: ScopeIssue, Number: 5}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrMalformed)
}

// ---------- Validate: capability (step 2) ----------

func TestValidateGrantExpired(t *testing.T) {
	g := testGrant(func(g *Grant) { g.ExpiresAt = testNow.Add(-time.Second) })
	_, err := Validate(openPREnvelope(t, g), g, testNow)
	assertErr(t, err, ErrGrantExpired)
}

func TestValidateKindNotGranted(t *testing.T) {
	g := testGrant(func(g *Grant) { g.AllowedKinds = []Kind{KindPostComment} })
	_, err := Validate(openPREnvelope(t, g), g, testNow)
	assertErr(t, err, ErrCapabilityMissing)
}

func TestValidateTokenScopeInsufficientForOpenPR(t *testing.T) {
	g := testGrant(func(g *Grant) { g.TokenScope = TokenReadOnly })
	_, err := Validate(openPREnvelope(t, g), g, testNow)
	assertErr(t, err, ErrCapabilityMissing)
}

func TestValidateTokenScopeThresholds(t *testing.T) {
	// post_review needs issues_write; contents_write outranks it, read_only does not.
	postReview := func(t *testing.T, scope TokenScope) error {
		t.Helper()
		g := testGrant(func(g *Grant) {
			g.AllowedKinds = []Kind{KindPostReview}
			g.TokenScope = scope
			g.Scope = Scope{Kind: ScopePullRequest, Number: 9, BaseRef: "main", HeadSHA: "x"}
		})
		env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindPostReview,
			Payload: mustJSON(t, PostReview{Verdict: VerdictComment})}
		_, err := Validate(env, g, testNow)
		return err
	}
	if err := postReview(t, TokenReadOnly); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatalf("read_only should not cover post_review, got %v", err)
	}
	if err := postReview(t, TokenIssuesWrite); err != nil {
		t.Fatalf("issues_write should cover post_review, got %v", err)
	}
	if err := postReview(t, TokenContentsWrite); err != nil {
		t.Fatalf("contents_write should cover post_review, got %v", err)
	}
}

// ---------- Validate: scope (step 3) ----------

func TestValidateOpenPRScopeIssue(t *testing.T) {
	g := testGrant() // issue scope #5
	got, err := Validate(openPREnvelope(t, g), g, testNow)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := got.(OpenPR); !ok {
		t.Fatalf("got %T, want OpenPR", got)
	}
}

func TestValidateOpenPRRejectedUnderPRScope(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.Scope = Scope{Kind: ScopePullRequest, Number: 9, BaseRef: "main", HeadSHA: "x"}
	})
	_, err := Validate(openPREnvelope(t, g), g, testNow)
	assertErr(t, err, ErrScopeMismatch)
}

func TestValidatePostReviewRejectedUnderScheduledScope(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.AllowedKinds = []Kind{KindPostReview}
		g.TokenScope = TokenIssuesWrite
		g.Scope = Scope{Kind: ScopeScheduled, BaseRef: "main"}
	})
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindPostReview,
		Payload: mustJSON(t, PostReview{Verdict: VerdictComment})}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrScopeMismatch)
}

func TestValidateNonscheduledScopeRequiresNumber(t *testing.T) {
	// issue scope with no number is malformed
	g := testGrant(func(g *Grant) { g.Scope = Scope{Kind: ScopeIssue, BaseRef: "main"} })
	_, err := Validate(openPREnvelope(t, g), g, testNow)
	assertErr(t, err, ErrMalformed)
}

// ---------- Payload decode + sanitisation (step 4) ----------

func TestOpenPRSanitisesTitleBody(t *testing.T) {
	g := testGrant()
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindOpenPR, Payload: mustJSON(t, OpenPR{
		Title: " Bump   acme   v1.1 ",
		Body:  "fixes @bob, closes #12 (but scoped to #5)",
	})}
	got, err := Validate(env, g, testNow)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	p := got.(OpenPR)
	if p.Title != "Bump acme v1.1" {
		t.Errorf("title = %q, want collapsed whitespace", p.Title)
	}
	if !strings.Contains(p.Body, "`@bob`") {
		t.Errorf("body = %q, want backticked mention", p.Body)
	}
	if !strings.Contains(p.Body, "ref #12") {
		t.Errorf("body = %q, want defanged cross-issue close", p.Body)
	}
}

func TestOpenPRMissingTitle(t *testing.T) {
	g := testGrant()
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindOpenPR, Payload: mustJSON(t, OpenPR{
		Title: "",
	})}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrMalformed)
}

func TestOpenPRUnknownFieldRejected(t *testing.T) {
	// Worktree/Topic were removed from the payload shape (schema v2); a stale
	// client smuggling one is a schema drift signal and must be rejected loudly.
	g := testGrant()
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindOpenPR, Payload: mustJSON(t, map[string]any{
		"title":    "Bump",
		"worktree": "cmd",
	})}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrMalformed)
}

// ---------- No-approve verdict (rules worth arguing about) ----------

func TestPostReviewRejectsApproveVerdict(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.AllowedKinds = []Kind{KindPostReview}
		g.TokenScope = TokenIssuesWrite
		g.Scope = Scope{Kind: ScopePullRequest, Number: 9, BaseRef: "main", HeadSHA: "x"}
	})
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindPostReview,
		Payload: json.RawMessage(`{"verdict":"approve","summary":"lgtm"}`)}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrMalformed)
}

func TestPostReviewCommentCountLimit(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.AllowedKinds = []Kind{KindPostReview}
		g.TokenScope = TokenIssuesWrite
		g.Scope = Scope{Kind: ScopePullRequest, Number: 9, BaseRef: "main", HeadSHA: "x"}
		g.Limits = DefaultLimits()
		g.Limits.MaxComments = 2
	})
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindPostReview, Payload: mustJSON(t, PostReview{
		Verdict: VerdictRequestChanges,
		Comments: []ReviewComment{
			{Path: "a.go", Line: 1, Side: "RIGHT", Body: "one"},
			{Path: "b.go", Line: 2, Side: "RIGHT", Body: "two"},
			{Path: "c.go", Line: 3, Side: "RIGHT", Body: "three"},
		},
	})}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrLimitExceeded)
}

func TestPostReviewDefaultsInvalidSideToRight(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.AllowedKinds = []Kind{KindPostReview}
		g.TokenScope = TokenIssuesWrite
		g.Scope = Scope{Kind: ScopePullRequest, Number: 9, BaseRef: "main", HeadSHA: "x"}
	})
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindPostReview, Payload: mustJSON(t, PostReview{
		Verdict:  VerdictComment,
		Comments: []ReviewComment{{Path: "a.go", Line: 3, Side: "WEIRD", Body: "x"}},
	})}
	got, err := Validate(env, g, testNow)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.(PostReview).Comments[0].Side != "RIGHT" {
		t.Errorf("side = %q, want RIGHT default", got.(PostReview).Comments[0].Side)
	}
}

func TestPostReviewZeroLineRejected(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.AllowedKinds = []Kind{KindPostReview}
		g.TokenScope = TokenIssuesWrite
		g.Scope = Scope{Kind: ScopePullRequest, Number: 9, BaseRef: "main", HeadSHA: "x"}
	})
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindPostReview, Payload: mustJSON(t, PostReview{
		Verdict:  VerdictComment,
		Comments: []ReviewComment{{Path: "a.go", Line: 0, Side: "RIGHT", Body: "x"}},
	})}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrMalformed)
}

func TestPostCommentEmptyBodyRejected(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.AllowedKinds = []Kind{KindPostComment}
		g.TokenScope = TokenIssuesWrite
	})
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindPostComment,
		Payload: json.RawMessage(`{"body":"   "}`)}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrMalformed)
}

func TestSetLabelsLimit(t *testing.T) {
	g := testGrant(func(g *Grant) {
		g.AllowedKinds = []Kind{KindSetLabels}
		g.TokenScope = TokenIssuesWrite
		g.Limits = DefaultLimits()
		g.Limits.MaxLabelsAdded = 2
	})
	env := Envelope{SchemaVersion: int(SchemaVersion), RunID: g.RunID, Kind: KindSetLabels, Payload: mustJSON(t, SetLabels{
		Add: []string{"a", "b", "c"},
	})}
	_, err := Validate(env, g, testNow)
	assertErr(t, err, ErrLimitExceeded)
}

// ---------- sanitiseBody rules ----------

func TestSanitiseBodyBackticksMentions(t *testing.T) {
	s := sanitiseBody("ping @alice and @bob-al and @al9", 1<<20, Scope{})
	want := "ping `@alice` and `@bob-al` and `@al9`"
	if s != want {
		t.Errorf("got %q, want %q", s, want)
	}
}

func TestSanitiseBodyDefangsClosingKeywords(t *testing.T) {
	s := sanitiseBody("this closes #12 and fixes #3 and resolves #100", 1<<20, Scope{Kind: ScopeIssue, Number: 5})
	for _, want := range []string{"ref #12", "ref #3", "ref #100"} {
		if !strings.Contains(s, want) {
			t.Errorf("got %q, want to contain %q", s, want)
		}
	}
	if strings.Contains(s, "closes") || strings.Contains(s, "fixes") || strings.Contains(s, "resolves") {
		t.Errorf("got %q, closing keywords should be defanged", s)
	}
}

func TestSanitiseBodyPreservesScopedIssueClose(t *testing.T) {
	// The exception: the run is scoped to issue #5, so "closes #5" is allowed.
	s := sanitiseBody("closes #5 done", 1<<20, Scope{Kind: ScopeIssue, Number: 5})
	if !strings.Contains(s, "closes #5") {
		t.Errorf("got %q, scoped close should be preserved", s)
	}
}

func TestSanitiseBodyStripsControlChars(t *testing.T) {
	s := sanitiseBody("a\x00b\x01c", 1<<20, Scope{})
	if s != "abc" {
		t.Errorf("got %q, want abc", s)
	}
}

func TestSanitiseBodyTruncates(t *testing.T) {
	s := sanitiseBody("hello world, this is quite long", 8, Scope{})
	if !strings.Contains(s, "[truncated]") {
		t.Errorf("got %q, want truncation marker", s)
	}
	if len(s) > 8+len("\n\n_[truncated]_") {
		t.Errorf("got %q len %d over budget", s, len(s))
	}
}

func TestSanitiseLineCollapsesWhitespace(t *testing.T) {
	if s := sanitiseLine("a   b\tc  d", 100); s != "a b c d" {
		t.Errorf("got %q", s)
	}
}

// ---------- globMatch ----------

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{".github/workflows/**", ".github/workflows/deploy.yml", true},
		{".github/workflows/**", "src/main.go", false},
		{"**/.npmrc", ".npmrc", true},
		{"**/.npmrc", "frontend/.npmrc", true},
		{"**/.npmrc", "src/main.go", false},
		{"CODEOWNERS", "CODEOWNERS", true},
		{"*.go", "main.go", true},
		{".git/**", ".git/config", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

// ---------- ValidateDiff (step 5) ----------

func TestValidateDiffEmptyRejected(t *testing.T) {
	err := ValidateDiff(Diff{}, testGrant())
	assertErr(t, err, ErrMalformed)
}

func TestValidateDiffTooManyFiles(t *testing.T) {
	g := testGrant()
	g.Limits.MaxFiles = 2
	err := ValidateDiff(Diff{Files: []string{"a", "b", "c"}}, g)
	assertErr(t, err, ErrLimitExceeded)
}

func TestValidateDiffTooManyLines(t *testing.T) {
	g := testGrant()
	g.Limits.MaxDiffLines = 10
	err := ValidateDiff(Diff{Files: []string{"a"}, AddedLines: 6, DeletedLines: 6}, g)
	assertErr(t, err, ErrLimitExceeded)
}

func TestValidateDiffDeniedDefaultPath(t *testing.T) {
	// even with empty repo-level DeniedPaths, defaults apply
	err := ValidateDiff(Diff{Files: []string{".github/workflows/deploy.yml"}}, testGrant())
	assertErr(t, err, ErrDeniedPath)
}

func TestValidateDiffDeniedRepoPath(t *testing.T) {
	g := testGrant(func(g *Grant) { g.DeniedPaths = []string{"secrets/**"} })
	err := ValidateDiff(Diff{Files: []string{"cmd/main.go", "secrets/x"}}, g)
	assertErr(t, err, ErrDeniedPath)
}

func TestValidateDiffOK(t *testing.T) {
	if err := ValidateDiff(Diff{Files: []string{"cmd/main.go"}, AddedLines: 5}, testGrant()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

// ---------- IdempotencyKey ----------

func TestIdempotencyKeyDeterministic(t *testing.T) {
	g := testGrant()
	env := openPREnvelope(t, g)
	a := IdempotencyKey(env, g)
	b := IdempotencyKey(env, g)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if len(a) != sha256.Size*2 {
		t.Fatalf("key len %d, want %d hex chars", len(a), sha256.Size*2)
	}
}

func TestIdempotencyKeyDiffersByPayload(t *testing.T) {
	g := testGrant()
	envs := []Envelope{
		openPREnvelope(t, g),
		openPREnvelope(t, g), // same structural, but we change payload body below
	}
	envs[1].Payload = mustJSON(t, OpenPR{Title: "Different", Body: "..."})
	if IdempotencyKey(envs[0], g) == IdempotencyKey(envs[1], g) {
		t.Fatal("different payloads must yield different idempotency keys")
	}
}
