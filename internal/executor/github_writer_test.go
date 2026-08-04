package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/maintainer-bot/internal/intent"
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
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			fmt.Fprint(w, `{"number":123,"html_url":"https://github.com/shambu2k/repo/pull/123"}`)
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

func TestWriterPostReview(t *testing.T) {
	w, rec := newAdapter(t, testToken)
	_, err := w.PostReview(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		PostReviewWrite{
			PRNumber: 9,
			Verdict:  intent.VerdictRequestChanges,
			Summary:  "needs work",
			Comments: []intent.ReviewComment{{Path: "a.go", Line: 3, Side: "RIGHT", Body: "here"}},
		})
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	r := (*rec)[0]
	if r.method != "POST" || r.path != "/repos/shambu2k/repo/pulls/9/reviews" {
		t.Fatalf("call = %s %s", r.method, r.path)
	}
	if r.body["event"] != "REQUEST_CHANGES" {
		t.Errorf("event = %v, want REQUEST_CHANGES", r.body["event"])
	}
	comments := r.body["comments"].([]any)
	c := comments[0].(map[string]any)
	if c["path"] != "a.go" || c["line"] != float64(3) || c["side"] != "RIGHT" || c["body"] != "here" {
		t.Errorf("comment = %v", c)
	}
}

func TestWriterPostReviewCommentEvent(t *testing.T) {
	w, rec := newAdapter(t, testToken)
	_, err := w.PostReview(t.Context(), Credential{Token: testToken, Repo: intent.Repo{Owner: "shambu2k", Name: "repo"}},
		PostReviewWrite{PRNumber: 9, Verdict: intent.VerdictComment, Summary: "ok"})
	if err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if (*rec)[0].body["event"] != "COMMENT" {
		t.Errorf("event = %v, want COMMENT", (*rec)[0].body["event"])
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
