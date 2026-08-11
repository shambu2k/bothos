package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var (
	ErrSchemaVersion     = errors.New("unknown schema version")
	ErrUnknownKind       = errors.New("unknown intent kind")
	ErrCapabilityMissing = errors.New("intent kind not granted for this run")
	ErrGrantExpired      = errors.New("grant expired")
	ErrScopeMismatch     = errors.New("intent kind not valid for run scope")
	ErrDeniedPath        = errors.New("diff touches a denied path")
	ErrLimitExceeded     = errors.New("limit exceeded")
	ErrMalformed         = errors.New("malformed payload")
)

// defaultDeniedPaths applies to every run regardless of repo config. These are
// the paths that turn a code-change capability into a privilege-escalation
// capability. Repo-level DeniedPaths are unioned with, never substituted for,
// this list.
var defaultDeniedPaths = []string{
	".github/workflows/**", // an agent that can edit CI can do anything CI can
	".github/actions/**",
	".git/**",
	"CODEOWNERS",
	".github/CODEOWNERS",
	"docs/CODEOWNERS",
	".gitattributes", // filter drivers execute on checkout
	"**/.npmrc",      // registry redirection
	"**/.pypirc",
	".bot/**", // the bot's own per-repo policy config
}

// Diff is what the executor computes from the sandbox worktree. The agent never
// supplies it.
type Diff struct {
	Files        []string
	AddedLines   int
	DeletedLines int
}

// Validate runs every check that does not require the diff. Call this first;
// it is cheap and rejects most bad intents before you spend a git operation.
// The returned value is the decoded, sanitised payload.
func Validate(env Envelope, g Grant, now time.Time) (any, error) {
	// 1. Structural.
	if env.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: got %d want %d", ErrSchemaVersion, env.SchemaVersion, SchemaVersion)
	}
	if env.RunID != g.RunID {
		return nil, fmt.Errorf("%w: envelope run %q, grant run %q", ErrScopeMismatch, env.RunID, g.RunID)
	}
	if !knownKind(env.Kind) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, env.Kind)
	}

	// 2. Capability. The grant was computed by policy before the agent ran, so
	// nothing the agent read can widen it.
	if now.After(g.ExpiresAt) {
		return nil, ErrGrantExpired
	}
	if !granted(env.Kind, g.AllowedKinds) {
		return nil, fmt.Errorf("%w: %q", ErrCapabilityMissing, env.Kind)
	}
	if err := tokenCovers(env.Kind, g.TokenScope); err != nil {
		return nil, err
	}

	// 3. Scope. A scheduled scan has no PR to review; an issue run has no PR to
	// update. Reject rather than silently retarget.
	if err := scopeAllows(env.Kind, g.Scope); err != nil {
		return nil, err
	}

	// 4. Payload decode + content sanitisation.
	return decodeAndSanitise(env, g)
}

// ValidateDiff runs after the executor has computed the diff from the worktree.
// Only open_pr and update_pr reach this.
func ValidateDiff(d Diff, g Grant) error {
	if len(d.Files) == 0 {
		return fmt.Errorf("%w: empty diff", ErrMalformed)
	}
	if len(d.Files) > g.Limits.MaxFiles {
		return fmt.Errorf("%w: %d files > %d", ErrLimitExceeded, len(d.Files), g.Limits.MaxFiles)
	}
	if n := d.AddedLines + d.DeletedLines; n > g.Limits.MaxDiffLines {
		return fmt.Errorf("%w: %d diff lines > %d", ErrLimitExceeded, n, g.Limits.MaxDiffLines)
	}
	denied := append(append([]string{}, defaultDeniedPaths...), g.DeniedPaths...)
	for _, f := range d.Files {
		for _, pat := range denied {
			if globMatch(pat, f) {
				return fmt.Errorf("%w: %q matches %q", ErrDeniedPath, f, pat)
			}
		}
	}
	return nil
}

// IdempotencyKey is derived by the executor, never supplied by the agent.
// River retries jobs and GitHub redelivers webhooks; without this you get
// duplicate PRs on the second attempt.
func IdempotencyKey(env Envelope, g Grant) string {
	canon, _ := json.Marshal(struct {
		Run     string          `json:"run"`
		Repo    Repo            `json:"repo"`
		Scope   Scope           `json:"scope"`
		Kind    Kind            `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}{g.RunID, g.Repo, g.Scope, env.Kind, env.Payload})
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// ---------- internals ----------

func knownKind(k Kind) bool {
	for _, v := range AllKinds {
		if v == k {
			return true
		}
	}
	return false
}

func granted(k Kind, allowed []Kind) bool {
	for _, v := range allowed {
		if v == k {
			return true
		}
	}
	return false
}

func tokenCovers(k Kind, ts TokenScope) error {
	need := map[Kind]TokenScope{
		KindOpenPR:      TokenContentsWrite,
		KindUpdatePR:    TokenContentsWrite,
		KindPostReview:  TokenIssuesWrite,
		KindPostComment: TokenIssuesWrite,
		KindSetLabels:   TokenIssuesWrite,
	}[k]
	rank := map[TokenScope]int{TokenReadOnly: 0, TokenIssuesWrite: 1, TokenContentsWrite: 2}
	if rank[ts] < rank[need] {
		return fmt.Errorf("%w: %q needs %q, run has %q", ErrCapabilityMissing, k, need, ts)
	}
	return nil
}

func scopeAllows(k Kind, s Scope) error {
	ok := map[Kind]map[ScopeKind]bool{
		KindOpenPR:      {ScopeIssue: true, ScopeScheduled: true},
		KindUpdatePR:    {ScopePullRequest: true},
		KindPostReview:  {ScopePullRequest: true},
		KindPostComment: {ScopeIssue: true, ScopePullRequest: true},
		KindSetLabels:   {ScopeIssue: true, ScopePullRequest: true},
	}[k]
	if !ok[s.Kind] {
		return fmt.Errorf("%w: %q under scope %q", ErrScopeMismatch, k, s.Kind)
	}
	if s.Kind != ScopeScheduled && s.Number == 0 {
		return fmt.Errorf("%w: scope %q with no number", ErrMalformed, s.Kind)
	}
	return nil
}

func decodeAndSanitise(env Envelope, g Grant) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(env.Payload)))
	dec.DisallowUnknownFields() // an unexpected field is a schema drift signal

	switch env.Kind {
	case KindOpenPR:
		var p OpenPR
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if strings.TrimSpace(p.Title) == "" {
			return nil, fmt.Errorf("%w: title required", ErrMalformed)
		}
		p.Title = sanitiseLine(p.Title, 120)
		p.Body = sanitiseBody(p.Body, g.Limits.MaxBodyBytes, g.Scope)
		return p, nil

	case KindUpdatePR:
		var p UpdatePR
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if p.Body != nil {
			b := sanitiseBody(*p.Body, g.Limits.MaxBodyBytes, g.Scope)
			p.Body = &b
		}
		return p, nil

	case KindPostReview:
		var p PostReview
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if p.Verdict != VerdictComment && p.Verdict != VerdictRequestChanges {
			return nil, fmt.Errorf("%w: verdict %q", ErrMalformed, p.Verdict)
		}
		if len(p.Comments) > g.Limits.MaxComments {
			return nil, fmt.Errorf("%w: %d comments > %d", ErrLimitExceeded, len(p.Comments), g.Limits.MaxComments)
		}
		p.Summary = sanitiseBody(p.Summary, g.Limits.MaxBodyBytes, g.Scope)
		for i := range p.Comments {
			if p.Comments[i].Side != "LEFT" && p.Comments[i].Side != "RIGHT" {
				p.Comments[i].Side = "RIGHT"
			}
			if p.Comments[i].Line < 1 {
				return nil, fmt.Errorf("%w: comment line %d", ErrMalformed, p.Comments[i].Line)
			}
			p.Comments[i].Body = sanitiseBody(p.Comments[i].Body, 4<<10, g.Scope)
			if p.Comments[i].Verified {
				p.Comments[i].Evidence = sanitiseBody(p.Comments[i].Evidence, 4<<10, g.Scope)
				if strings.TrimSpace(p.Comments[i].Evidence) == "" {
					return nil, fmt.Errorf("%w: verified comment evidence required", ErrMalformed)
				}
			} else {
				p.Comments[i].Evidence = ""
			}
		}
		return p, nil

	case KindPostComment:
		var p PostComment
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		p.Body = sanitiseBody(p.Body, g.Limits.MaxBodyBytes, g.Scope)
		if strings.TrimSpace(p.Body) == "" {
			return nil, fmt.Errorf("%w: empty comment", ErrMalformed)
		}
		return p, nil

	case KindSetLabels:
		var p SetLabels
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if len(p.Add) > g.Limits.MaxLabelsAdded {
			return nil, fmt.Errorf("%w: %d labels > %d", ErrLimitExceeded, len(p.Add), g.Limits.MaxLabelsAdded)
		}
		return p, nil
	}
	return nil, ErrUnknownKind
}

var (
	mentionRe = regexp.MustCompile(`@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))`)
	// GitHub auto-closes issues on these keywords in a PR body. An injected
	// changelog that says "closes #1" should not close your issue #1.
	closingRe = regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)\s+#(\d+)\b`)
)

// sanitiseBody neutralises the two ways body text acts on GitHub rather than
// just appearing on it: mass-mentions and issue-closing keywords.
func sanitiseBody(s string, maxBytes int, sc Scope) string {
	s = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)

	// Backtick mentions so they render but do not notify.
	s = mentionRe.ReplaceAllString(s, "`@$1`")

	// Strip closing keywords unless they name the very issue this run is for.
	s = closingRe.ReplaceAllStringFunc(s, func(m string) string {
		g := closingRe.FindStringSubmatch(m)
		if sc.Kind == ScopeIssue && g[3] == fmt.Sprint(sc.Number) {
			return m
		}
		return "ref #" + g[3]
	})

	if len(s) > maxBytes {
		s = s[:maxBytes] + "\n\n_[truncated]_"
	}
	return s
}

func sanitiseLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// globMatch supports the leading-**/ and trailing-/** forms used above.
// Swap for a real gitignore matcher (e.g. github.com/gobwas/glob) in anger.
func globMatch(pattern, name string) bool {
	switch {
	case strings.HasSuffix(pattern, "/**"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "**"))
	case strings.HasPrefix(pattern, "**/"):
		base := strings.TrimPrefix(pattern, "**/")
		return name == base || strings.HasSuffix(name, "/"+base)
	default:
		ok, _ := path.Match(pattern, name)
		return ok
	}
}
