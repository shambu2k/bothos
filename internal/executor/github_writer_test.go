package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/intent"
)

type recordedRequest struct {
	method, path, auth string
	body               map[string]any
}

// newFakeGitHub spins up an httptest server that mimics the handful of GitHub
// REST endpoints the writer uses, recording method/path/auth/body for every
// call. The returned client sends whatever token it was built with as the
// Authorization header — which is exactly what the test asserts.
func newFakeGitHub(t *testing.T) (*github.Client, *[]recordedRequest) {
	t.Helper()
	var rec []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		rec = append(rec, recordedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: m})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"bothos-bot"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			fmt.Fprint(w, `{"number":123,"html_url":"https://github.com/shambu2k/repo/pull/123"}`)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/comments/"):
			fmt.Fprint(w, `{"id":77}`)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `{"id":7}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `{"id":9}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(204)
		default:
			http.Error(w, "unexpected: "+r.Method+" "+r.URL.Path, 500)
		}
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL + "/")
	client := github.NewClient(server.Client())
	client.BaseURL = u
	return client, &rec
}

func newAdapter(t *testing.T, token string) (GitHubWriter, *[]recordedRequest) {
	t.Helper()
	base, rec := newFakeGitHub(t)
	return NewGitHubWriter(func(pat string) *github.Client {
		if pat != token {
			t.Fatalf("adapter resolved token %q, want %q", pat, token)
		}
		c := github.NewClient(base.Client()).WithAuthToken(pat)
		c.BaseURL = base.BaseURL
		return c
	}), rec
}

const testToken = "ghp_fakeTestPat123"

func TestWriterOpenPR(t *testing.T) {
	w, rec := newAdapter(t, testToken)
	ref, err := w.OpenPR(t.Context(), Credential{
		AccountID: "acct-1", Scope: intent.TokenContentsWrite, Token: testToken,
		Repo: intent.Repo{Owner: "shambu2k", Name: "repo"},
	}, OpenPRWrite{
		Branch: "bot/run-1-bump-dep", Base: "main",
		Title: "Bump acme", Body: "hello", Draft: false,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if ref != "shambu2k/repo#123" {
		t.Fatalf("ref = %q, want shambu2k/repo#123", ref)
	}
	r := (*rec)[0]
	if r.method != "POST" || r.path != "/repos/shambu2k/repo/pulls" {
		t.Fatalf("call = %s %s, want POST /repos/shambu2k/repo/pulls", r.method, r.path)
	}
	if r.auth != "Bearer "+testToken {
		t.Fatalf("auth = %q, want Bearer %s", r.auth, testToken)
	}
	if got := r.body["head"]; got != "bot/run-1-bump-dep" {
		t.Errorf("head = %v", got)
	}
	if got := r.body["base"]; got != "main" {
		t.Errorf("base = %v", got)
	}
	if got := r.body["title"]; got != "Bump acme" {
		t.Errorf("title = %v", got)
	}
	if got := r.body["draft"]; got != false {
		t.Errorf("draft = %v", got)
	}
}

func TestWriterUpdatePR(t *testing.T) {
	w, rec := newAdapter(t, testToken)
	body := "updated body"
	draft := true
	_, err := w.UpdatePR(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		UpdatePRWrite{PRNumber: 9, Body: &body, Draft: &draft})
	if err != nil {
		t.Fatalf("UpdatePR: %v", err)
	}
	r := (*rec)[0]
	if r.method != "PATCH" || r.path != "/repos/shambu2k/repo/pulls/9" {
		t.Fatalf("call = %s %s, want PATCH /repos/shambu2k/repo/pulls/9", r.method, r.path)
	}
	if r.body["body"] != "updated body" || r.body["draft"] != true {
		t.Errorf("update body = %v (want updated body, true)", r.body)
	}
}

func TestWriterPostReviewCreatesAggregateComment(t *testing.T) {
	writer, requests := newAdapter(t, testToken)
	ref, commentID, err := writer.PostReview(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		PostReviewWrite{
			PRNumber: 9,
			Verdict:  intent.VerdictRequestChanges,
			Summary:  "needs <!-- hidden --> work",
			Comments: []intent.ReviewComment{
				{Path: "package.json", Line: 3, Side: "RIGHT", Body: "dependency_delta: tar changed", Verified: true, Evidence: "<token>"},
				{Path: "a.go", Line: 8, Side: "RIGHT", Body: "consider <!-- marker --> this"},
			},
		})
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if ref != "shambu2k/repo#9" || commentID != 9 {
		t.Fatalf("ref=%q commentID=%d", ref, commentID)
	}
	if len(*requests) != 3 {
		t.Fatalf("requests = %+v", *requests)
	}
	create := (*requests)[2]
	if create.method != http.MethodPost || create.path != "/repos/shambu2k/repo/issues/9/comments" {
		t.Fatalf("create = %+v", create)
	}
	body, _ := create.body["body"].(string)
	for _, want := range []string{
		reviewCommentMarker,
		"## Bothos review",
		"### [opinion] Summary",
		"### [verified] Deterministic findings",
		"[verified] dependency_delta: tar changed",
		"&lt;token&gt;",
		"### [opinion] Model comments",
		"[opinion] consider &lt;!-- marker --&gt; this",
		"[opinion] Recommendation: request changes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "[verified] dependency_delta") > strings.Index(body, "[opinion] consider") {
		t.Fatalf("opinion rendered before verified finding:\n%s", body)
	}
	for _, request := range *requests {
		if strings.Contains(request.path, "/pulls/9/reviews") {
			t.Fatalf("review approval endpoint called: %+v", request)
		}
	}
}

func TestWriterPostReviewEditsMappedComment(t *testing.T) {
	writer, requests := newAdapter(t, testToken)
	_, commentID, err := writer.PostReview(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		PostReviewWrite{PRNumber: 9, CommentID: 77, Verdict: intent.VerdictComment, Summary: "updated"})
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if commentID != 77 || len(*requests) != 1 {
		t.Fatalf("commentID=%d requests=%+v", commentID, *requests)
	}
	if request := (*requests)[0]; request.method != http.MethodPatch || request.path != "/repos/shambu2k/repo/issues/comments/77" {
		t.Fatalf("edit = %+v", request)
	}
}

func TestWriterPostReviewRecoversOwnedMarker(t *testing.T) {
	edits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"bothos-bot"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[{"id":55,"body":"<!-- bothos-pr-review -->\nqueued","user":{"login":"bothos-bot"}}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/comments/55"):
			edits++
			fmt.Fprint(w, `{"id":55}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	writer := NewGitHubWriter(func(string) *github.Client {
		client := github.NewClient(server.Client())
		client.BaseURL = base
		return client
	})
	cred := Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}}
	_, commentID, err := writer.PostReview(t.Context(), cred, PostReviewWrite{PRNumber: 9, Verdict: intent.VerdictComment})
	if err != nil || commentID != 55 || edits != 1 {
		t.Fatalf("commentID=%d edits=%d err=%v", commentID, edits, err)
	}
}

func TestWriterPostReviewReplacesDeletedMappedComment(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/comments/77"):
			http.Error(w, "deleted", http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			creates++
			fmt.Fprint(w, `{"id":88}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL + "/")
	writer := NewGitHubWriter(func(string) *github.Client {
		client := github.NewClient(server.Client())
		client.BaseURL = base
		return client
	})
	cred := Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}}
	_, commentID, err := writer.PostReview(t.Context(), cred, PostReviewWrite{PRNumber: 9, CommentID: 77, Verdict: intent.VerdictComment})
	if err != nil || commentID != 88 || creates != 1 {
		t.Fatalf("commentID=%d creates=%d err=%v", commentID, creates, err)
	}
}

func TestWriterPostComment(t *testing.T) {
	w, rec := newAdapter(t, testToken)
	_, err := w.PostComment(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		PostCommentWrite{Number: 5, Body: "done"})
	if err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	r := (*rec)[0]
	if r.method != "POST" || r.path != "/repos/shambu2k/repo/issues/5/comments" {
		t.Fatalf("call = %s %s", r.method, r.path)
	}
	if r.body["body"] != "done" {
		t.Errorf("body = %v", r.body["body"])
	}
}

func TestWriterAcknowledgeReviewCreatesExactMarker(t *testing.T) {
	writer, requests := newAdapter(t, testToken)
	cred := Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}}
	if _, _, err := writer.AcknowledgeReview(context.Background(), cred, 42); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 3 {
		t.Fatalf("requests = %+v", *requests)
	}
	create := (*requests)[2]
	if create.method != http.MethodPost || create.path != "/repos/shambu2k/repo/issues/42/comments" ||
		create.body["body"] != "<!-- bothos-pr-review -->\n👀 Review queued." {
		t.Fatalf("create = %+v", create)
	}
}

func TestWriterAcknowledgeReviewOnlyTrustsOwnedMarker(t *testing.T) {
	for _, tt := range []struct {
		name        string
		author      string
		wantCreates int
	}{
		{name: "owned", author: "bothos-bot", wantCreates: 0},
		{name: "attacker", author: "attacker", wantCreates: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			creates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/user":
					fmt.Fprint(w, `{"login":"bothos-bot"}`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
					fmt.Fprintf(w, `[{"id":9,"body":"<!-- bothos-pr-review -->","user":{"login":%q}}]`, tt.author)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
					creates++
					fmt.Fprint(w, `{"id":10}`)
				default:
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL + "/")
			writer := NewGitHubWriter(func(string) *github.Client {
				client := github.NewClient(server.Client())
				client.BaseURL = base
				return client
			})
			cred := Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}}
			if _, _, err := writer.AcknowledgeReview(context.Background(), cred, 42); err != nil {
				t.Fatal(err)
			}
			if creates != tt.wantCreates {
				t.Fatalf("creates = %d, want %d", creates, tt.wantCreates)
			}
		})
	}
}

func TestWriterSetLabels(t *testing.T) {
	w, rec := newAdapter(t, testToken)
	_, err := w.SetLabels(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		SetLabelsWrite{Number: 5, Add: []string{"kind/upgrade"}, Remove: []string{"needs-triage"}})
	if err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	if len(*rec) != 2 {
		t.Fatalf("calls = %d, want 2 (add + remove)", len(*rec))
	}
	add, rm := (*rec)[0], (*rec)[1]
	if add.method != "POST" || add.path != "/repos/shambu2k/repo/issues/5/labels" {
		t.Fatalf("add call = %s %s", add.method, add.path)
	}
	if rm.method != "DELETE" || rm.path != "/repos/shambu2k/repo/issues/5/labels/needs-triage" {
		t.Fatalf("remove call = %s %s", rm.method, rm.path)
	}
}

func TestWriterNeverPutsTokenInURLOrBody(t *testing.T) {
	w, rec := newAdapter(t, testToken)
	_, err := w.OpenPR(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		OpenPRWrite{Branch: "bot/r-x", Base: "main", Title: "x"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	for _, r := range *rec {
		if strings.Contains(r.path, testToken) {
			t.Errorf("token leaked into path: %s", r.path)
		}
		if b, _ := json.Marshal(r.body); strings.Contains(string(b), testToken) {
			t.Errorf("token leaked into body: %s", b)
		}
	}
}
