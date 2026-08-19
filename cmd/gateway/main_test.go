package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/riverqueue/river"
	"github.com/shambu2k/bothos/internal/dispatch"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/policy"
	"github.com/shambu2k/bothos/internal/queue"
	"github.com/shambu2k/bothos/internal/testdb"
)

const e2eSecret = "e2e-secret"

// TestEndToEndWebhookToWorker exercises the whole Phase 0 spine in-process:
// a signed webhook -> signature validation -> policy decision -> run row +
// enqueued job -> River worker marks the run succeeded.
func TestEndToEndWebhookToWorker(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.DSN(t)

	l, err := ledger.New(ctx, dsn)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	defer l.Close()
	if err := l.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A real worker handler: mark running, then succeeded (Phase 0 stub).
	handler := func(ctx context.Context, runID string) error {
		_ = l.SetRunStatus(ctx, runID, ledger.RunRunning)
		return l.SetRunStatus(ctx, runID, ledger.RunSucceeded)
	}
	q, err := queue.Open(ctx, dsn, map[string]river.QueueConfig{"default": {MaxWorkers: 2}}, handler)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	defer q.Close()
	// Truncate AFTER migrations created the tables (ledger + River).
	testdb.Truncate(t, dsn, "runs", "intents", "river_job")
	if err := q.Client().Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = q.Client().Stop(ctx) }()

	d := dispatch.New(l, q, func(ctx context.Context, owner, name string) (policy.Rules, error) {
		return policy.Rules{Enabled: true, AllowedLabels: []string{"kind/upgrade"}, ActorAllowlist: []string{"shambu2k"}}, nil
	}, func(context.Context, string, string, string) (bool, error) {
		return true, nil
	}, nil)

	srv := httptest.NewServer(webhookHandler(e2eSecret, d))
	defer srv.Close()

	// A labeled-issue webhook that policy allows.
	payload := issuesPayload("shambu2k")
	req := signedRequest(t, srv.URL+"/webhook", "issues", payload)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The worker should mark an allowed run succeeded within a few seconds.
	deadline := time.Now().Add(8 * time.Second)
	for {
		var status string
		err := q.Pool().QueryRow(ctx,
			`SELECT status FROM runs WHERE decision='allow' ORDER BY created_at DESC LIMIT 1`).Scan(&status)
		if err == nil && status == string(ledger.RunSucceeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached succeeded (last err=%v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// And a run that policy denies (unauthorized actor) must be recorded as
	// denied with no job.
	req = signedRequest(t, srv.URL+"/webhook", "issues", issuesPayload("attacker"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("denied post: status=%v err=%v", resp.StatusCode, err)
	} else {
		resp.Body.Close()
	}
	var denyStatus string
	if err := q.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE decision='deny' ORDER BY created_at DESC LIMIT 1`).Scan(&denyStatus); err != nil {
		t.Fatalf("denied run not recorded: %v", err)
	}
	if denyStatus != string(ledger.RunDenied) {
		t.Fatalf("denied status = %q, want denied", denyStatus)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.DSN(t)
	l, _ := ledger.New(ctx, dsn)
	defer l.Close()
	q, _ := queue.Open(ctx, dsn, nil, nil)
	defer q.Close()
	d := dispatch.New(l, q, func(ctx context.Context, o, n string) (policy.Rules, error) {
		return policy.Rules{Enabled: true}, nil
	}, nil, nil)
	srv := httptest.NewServer(webhookHandler(e2eSecret, d))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestActorAuthorizerPermissionMapping(t *testing.T) {
	for _, tt := range []struct {
		permission string
		want       bool
	}{
		{permission: "admin", want: true},
		{permission: "maintain", want: true},
		{permission: "write", want: true},
		{permission: "triage", want: false},
		{permission: "read", want: false},
		{permission: "unknown", want: false},
	} {
		t.Run(tt.permission, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/collaborators/alice/permission" {
					t.Fatalf("path = %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"permission":"` + tt.permission + `"}`))
			}))
			defer server.Close()
			client := github.NewClient(server.Client())
			client.BaseURL, _ = url.Parse(server.URL + "/")

			got, err := newActorAuthorizer(client, true)(context.Background(), "o", "r", "alice")
			if err != nil || got != tt.want {
				t.Fatalf("authorized=%v err=%v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestActorAuthorizerMissingTokenAndAPIFailureDeny(t *testing.T) {
	got, err := newActorAuthorizer(nil, false)(context.Background(), "o", "r", "alice")
	if err != nil || got {
		t.Fatalf("missing token authorized=%v err=%v", got, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()
	client := github.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")
	got, err = newActorAuthorizer(client, true)(context.Background(), "o", "r", "alice")
	if err == nil || got {
		t.Fatalf("API failure authorized=%v err=%v", got, err)
	}
}

func TestPullRequestLoaderReturnsImmutableSHAs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/12" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"base":{"sha":"base-sha"},"head":{"sha":"head-sha"}}`))
	}))
	defer server.Close()
	client := github.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")

	base, head, err := newPullRequestLoader(client)(context.Background(), "o", "r", 12)
	if err != nil || base != "base-sha" || head != "head-sha" {
		t.Fatalf("base=%q head=%q err=%v", base, head, err)
	}
}

func TestGatewayMissingRepositoryConfigDeniesWithoutEnqueue(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.DSN(t)
	ledgerStore, err := ledger.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer ledgerStore.Close()
	if err := ledgerStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	q, err := queue.Open(ctx, dsn, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	testdb.Truncate(t, dsn, "repo_config", "runs", "intents", "river_job")

	dispatcher := dispatch.New(ledgerStore, q, ledgerStore.RulesForRepo, nil, nil)
	server := httptest.NewServer(webhookHandler(e2eSecret, dispatcher))
	defer server.Close()
	payload, _ := json.Marshal(map[string]any{
		"action":     "opened",
		"number":     9,
		"repository": map[string]any{"name": "unconfigured", "owner": map[string]any{"login": "acme"}},
		"pull_request": map[string]any{
			"number": 9,
			"base":   map[string]any{"ref": "main", "sha": "base-sha"},
			"head":   map[string]any{"ref": "feature", "sha": "head-sha"},
		},
		"sender": map[string]any{"login": "alice", "type": "User"},
	})
	response, err := http.DefaultClient.Do(signedRequest(t, server.URL+"/webhook", "pull_request", payload))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var denied, jobs int
	if err := q.Pool().QueryRow(ctx, `SELECT count(*) FROM runs WHERE decision='deny'`).Scan(&denied); err != nil {
		t.Fatal(err)
	}
	if err := q.Pool().QueryRow(ctx, `SELECT count(*) FROM river_job`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if denied != 1 || jobs != 0 {
		t.Fatalf("denied=%d jobs=%d", denied, jobs)
	}
}

func issuesPayload(actor string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": "labeled",
		"issue":  map[string]any{"number": 5},
		"label":  map[string]any{"name": "kind/upgrade"},
		"sender": map[string]any{"login": actor},
		"repository": map[string]any{
			"name":  "repo",
			"owner": map[string]any{"login": "shambu2k"},
		},
	})
	return b
}

func signedRequest(t *testing.T, url, eventType string, payload []byte) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(e2eSecret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventType)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-Hub-Signature", "sha1="+sig)
	return req
}
