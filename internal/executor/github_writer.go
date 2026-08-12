package executor

import (
	"context"
	"fmt"
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

func (w *githubWriter) PostReview(ctx context.Context, cred Credential, spec PostReviewWrite) (string, error) {
	client := w.newClient(cred.Token)
	event := "COMMENT"
	if spec.Verdict == intent.VerdictRequestChanges {
		event = "REQUEST_CHANGES"
	}
	comments := make([]*github.DraftReviewComment, 0, len(spec.Comments))
	for _, c := range spec.Comments {
		comments = append(comments, &github.DraftReviewComment{
			Path: github.String(c.Path),
			Line: github.Int(c.Line),
			Side: github.String(c.Side),
			Body: github.String(c.Body),
		})
	}
	_, _, err := client.PullRequests.CreateReview(ctx, cred.Repo.Owner, cred.Repo.Name, spec.PRNumber, &github.PullRequestReviewRequest{
		Body:     github.String(spec.Summary),
		Event:    github.String(event),
		Comments: comments,
	})
	if err != nil {
		return "", err
	}
	return w.ref(cred, spec.PRNumber), nil
}

func (w *githubWriter) AcknowledgeReview(ctx context.Context, cred Credential, prNumber int) (string, error) {
	client := w.newClient(cred.Token)
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return "", err
	}
	login := user.GetLogin()
	if login == "" {
		return "", fmt.Errorf("authenticated GitHub login is empty")
	}

	options := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		comments, response, err := client.Issues.ListComments(ctx, cred.Repo.Owner, cred.Repo.Name, prNumber, options)
		if err != nil {
			return "", err
		}
		for _, comment := range comments {
			if strings.EqualFold(comment.GetUser().GetLogin(), login) && strings.Contains(comment.GetBody(), reviewCommentMarker) {
				return w.ref(cred, prNumber), nil
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	if _, _, err := client.Issues.CreateComment(ctx, cred.Repo.Owner, cred.Repo.Name, prNumber, &github.IssueComment{
		Body: github.String(reviewQueuedBody),
	}); err != nil {
		return "", err
	}
	return w.ref(cred, prNumber), nil
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
