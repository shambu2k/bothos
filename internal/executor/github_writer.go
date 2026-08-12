package executor

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"os/exec"
	"strings"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/intent"
)

// githubWriter is the thin network adapter over go-github. It is the only code
// that turns a Credential into an HTTP call, and it never sees the grant or
// the agent — it receives already-resolved write specs.
type githubWriter struct {
	newClient func(token string) *github.Client
}

const (
	reviewCommentMarker = "<!-- bothos-pr-review -->"
	reviewQueuedBody    = reviewCommentMarker + "\n👀 Review queued."
)

// NewGitHubWriter returns a GitHubWriter backed by go-github. newClient is
// injectable for tests (override BaseURL to an httptest server); when nil it
// builds a production client authenticating with the credential's PAT.
func NewGitHubWriter(newClient func(token string) *github.Client) GitHubWriter {
	if newClient == nil {
		newClient = func(token string) *github.Client {
			return github.NewClient(nil).WithAuthToken(token)
		}
	}
	return &githubWriter{newClient: newClient}
}

func (w *githubWriter) ref(cred Credential, number int) string {
	return fmt.Sprintf("%s/%s#%d", cred.Repo.Owner, cred.Repo.Name, number)
}

// PushBranch pushes a locally-committed branch (in worktree) to the remote using
// the executable's git + the credential's PAT. It is the executor's push step:
// the worker commits locally, this pushes, then OpenPR targets the branch.
func (w *githubWriter) PushBranch(ctx context.Context, cred Credential, branch, worktree string) error {
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		cred.Token, cred.Repo.Owner, cred.Repo.Name)
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "push", url, branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push %s: %w: %s", branch, err, out)
	}
	return nil
}

func (w *githubWriter) OpenPR(ctx context.Context, cred Credential, spec OpenPRWrite) (string, error) {
	client := w.newClient(cred.Token)
	pr, _, err := client.PullRequests.Create(ctx, cred.Repo.Owner, cred.Repo.Name, &github.NewPullRequest{
		Title: github.String(spec.Title),
		Head:  github.String(spec.Branch),
		Base:  github.String(spec.Base),
		Body:  github.String(spec.Body),
		Draft: github.Bool(spec.Draft),
	})
	if err != nil {
		return "", err
	}
	return w.ref(cred, pr.GetNumber()), nil
}

func (w *githubWriter) UpdatePR(ctx context.Context, cred Credential, spec UpdatePRWrite) (string, error) {
	client := w.newClient(cred.Token)
	// go-github's PullRequests.Edit builds a pullRequestUpdate struct that
	// omits Draft, so the high-level path cannot change the draft flag. Use
	// the client's documented NewRequest/Do primitives (the same ones go-github
	// methods are built on) with the exact REST fields instead.
	update := map[string]any{}
	if spec.Body != nil {
		update["body"] = *spec.Body
	}
	if spec.Draft != nil {
		update["draft"] = *spec.Draft
	}
	if len(update) > 0 {
		u := fmt.Sprintf("repos/%v/%v/pulls/%d", cred.Repo.Owner, cred.Repo.Name, spec.PRNumber)
		req, err := client.NewRequest(http.MethodPatch, u, update)
		if err != nil {
			return "", err
		}
		if _, err := client.Do(ctx, req, nil); err != nil {
			return "", err
		}
	}
	return w.ref(cred, spec.PRNumber), nil
}

func (w *githubWriter) PostReview(ctx context.Context, cred Credential, spec PostReviewWrite) (string, int64, error) {
	client := w.newClient(cred.Token)
	body := renderPostReview(spec)
	commentID := spec.CommentID
	if commentID == 0 {
		var err error
		commentID, err = findOwnedReviewComment(ctx, client, cred, spec.PRNumber)
		if err != nil {
			return "", 0, err
		}
	}
	if commentID != 0 {
		comment, response, err := client.Issues.EditComment(ctx, cred.Repo.Owner, cred.Repo.Name, commentID, &github.IssueComment{
			Body: github.String(body),
		})
		if err == nil {
			return w.ref(cred, spec.PRNumber), comment.GetID(), nil
		}
		if response == nil || response.StatusCode != http.StatusNotFound {
			return "", 0, err
		}
	}

	comment, _, err := client.Issues.CreateComment(ctx, cred.Repo.Owner, cred.Repo.Name, spec.PRNumber, &github.IssueComment{
		Body: github.String(body),
	})
	if err != nil {
		return "", 0, err
	}
	return w.ref(cred, spec.PRNumber), comment.GetID(), nil
}

// PostReviewComments places each review remark as a real inline PR review
// comment anchored at its path:line:side on the head commit (go-github's
// PullRequests.CreateComment, which renders in the diff view). Remarks with no
// path or line are skipped, and a remark that targets a line outside the
// current diff hunk (GitHub returns 422 after a push shifts lines) is skipped
// too — a stale anchor must never fail the whole review. The remaining
// findings still live in the persistent summary comment.
func (w *githubWriter) PostReviewComments(ctx context.Context, cred Credential, spec PostReviewWrite) (int, error) {
	if spec.PRNumber == 0 || cred.Repo.Owner == "" || cred.Repo.Name == "" {
		return 0, nil
	}
	client := w.newClient(cred.Token)
	posted := 0
	for _, c := range spec.Comments {
		if c.Path == "" || c.Line <= 0 {
			continue // nothing to anchor inline
		}
		side := c.Side
		if side == "" {
			side = "RIGHT"
		}
		cc, res, err := client.PullRequests.CreateComment(ctx, cred.Repo.Owner, cred.Repo.Name, spec.PRNumber, &github.PullRequestComment{
			CommitID:  github.String(spec.HeadSHA),
			Path:      github.String(c.Path),
			Line:      github.Int(c.Line),
			Side:      github.String(side),
			Body:      github.String(renderInlineReviewComment(c)),
			InReplyTo: nil,
		})
		if err != nil {
			if res != nil && res.StatusCode == http.StatusUnprocessableEntity {
				// Line not in the current diff hunk (stale after a push).
				continue
			}
			return posted, err
		}
		_ = cc
		posted++
	}
	return posted, nil
}

// renderInlineReviewComment renders one remark as an inline PR review comment.
// The [verified]/[opinion] tag travels in the body so readers on the diff
// still see provenance without opening the summary.
func renderInlineReviewComment(c intent.ReviewComment) string {
	var b strings.Builder
	if c.Verified {
		b.WriteString("[verified] ")
	} else {
		b.WriteString("[opinion] ")
	}
	b.WriteString(c.Body)
	if c.Verified && c.Evidence != "" {
		b.WriteString("\n\n<details><summary>Evidence</summary>\n\n<pre>")
		b.WriteString(html.EscapeString(c.Evidence))
		b.WriteString("</pre>\n</details>")
	}
	return b.String()
}

func (w *githubWriter) AcknowledgeReview(ctx context.Context, cred Credential, prNumber int) (string, int64, error) {
	client := w.newClient(cred.Token)
	commentID, err := findOwnedReviewComment(ctx, client, cred, prNumber)
	if err != nil {
		return "", 0, err
	}
	if commentID != 0 {
		return w.ref(cred, prNumber), commentID, nil
	}
	comment, _, err := client.Issues.CreateComment(ctx, cred.Repo.Owner, cred.Repo.Name, prNumber, &github.IssueComment{
		Body: github.String(reviewQueuedBody),
	})
	if err != nil {
		return "", 0, err
	}
	return w.ref(cred, prNumber), comment.GetID(), nil
}

func findOwnedReviewComment(ctx context.Context, client *github.Client, cred Credential, prNumber int) (int64, error) {
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return 0, err
	}
	login := user.GetLogin()
	if login == "" {
		return 0, fmt.Errorf("authenticated GitHub login is empty")
	}

	options := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		comments, response, err := client.Issues.ListComments(ctx, cred.Repo.Owner, cred.Repo.Name, prNumber, options)
		if err != nil {
			return 0, err
		}
		for _, comment := range comments {
			if strings.EqualFold(comment.GetUser().GetLogin(), login) && strings.Contains(comment.GetBody(), reviewCommentMarker) {
				return comment.GetID(), nil
			}
		}
		if response.NextPage == 0 {
			return 0, nil
		}
		options.Page = response.NextPage
	}
}

func renderPostReview(spec PostReviewWrite) string {
	var body strings.Builder
	body.WriteString(reviewCommentMarker)
	body.WriteString("\n## Bothos review\n\n")
	body.WriteString(html.EscapeString(spec.Summary))
	body.WriteString("\n")
	if spec.Verdict == intent.VerdictRequestChanges {
		body.WriteString("\n[opinion] Recommendation: request changes\n")
	}

	verified, opinions := 0, 0
	for _, c := range spec.Comments {
		if c.Verified {
			verified++
		} else {
			opinions++
		}
	}
	if verified+opinions == 0 {
		body.WriteString("\nNo inline remarks.\n")
		return body.String()
	}

	body.WriteString("\n### Inline remarks (on the diff)\n")
	for _, c := range spec.Comments {
		tag := "opinion"
		if c.Verified {
			tag = "verified"
		}
		if c.Path == "" || c.Line <= 0 {
			// No anchorable line: keep it here rather than dropping it.
			fmt.Fprintf(&body, "\n- [%s] %s\n", tag, html.EscapeString(c.Body))
			continue
		}
		fmt.Fprintf(&body, "\n- [%s] `%s:%d` — %s\n", tag, html.EscapeString(c.Path), c.Line, html.EscapeString(c.Body))
		if c.Verified && c.Evidence != "" {
			body.WriteString("  <details><summary>Evidence</summary>\n\n  <pre>")
			body.WriteString(html.EscapeString(c.Evidence))
			body.WriteString("</pre>\n  </details>\n")
		}
	}
	return body.String()
}

func (w *githubWriter) PostComment(ctx context.Context, cred Credential, spec PostCommentWrite) (string, error) {
	client := w.newClient(cred.Token)
	if _, _, err := client.Issues.CreateComment(ctx, cred.Repo.Owner, cred.Repo.Name, spec.Number, &github.IssueComment{
		Body: github.String(spec.Body),
	}); err != nil {
		return "", err
	}
	return w.ref(cred, spec.Number), nil
}

func (w *githubWriter) SetLabels(ctx context.Context, cred Credential, spec SetLabelsWrite) (string, error) {
	client := w.newClient(cred.Token)
	if len(spec.Add) > 0 {
		if _, _, err := client.Issues.AddLabelsToIssue(ctx, cred.Repo.Owner, cred.Repo.Name, spec.Number, spec.Add); err != nil {
			return "", err
		}
	}
	for _, label := range spec.Remove {
		if _, err := client.Issues.RemoveLabelForIssue(ctx, cred.Repo.Owner, cred.Repo.Name, spec.Number, label); err != nil {
			return "", err
		}
	}
	return w.ref(cred, spec.Number), nil
}
