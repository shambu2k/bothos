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
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/shambu2k/maintainer-bot/internal/dispatch"
	"github.com/shambu2k/maintainer-bot/internal/ledger"
	"github.com/shambu2k/maintainer-bot/internal/policy"
	"github.com/shambu2k/maintainer-bot/internal/queue"
	"github.com/shambu2k/maintainer-bot/internal/testdb"
)

const e2eSecret = "e2e-secret"

// TestEndToEndWebhookToWorker exercises the whole Phase 0 spine in-process:
// a signed webhook -> signature validation -> policy decision -> run row +
// enqueued job -> River worker marks the run succeeded.
func TestEndToEndWebhookToWorker(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.DSN(t)
	testdb.Truncate(t, dsn, "runs", "intents", "river_job")

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
	if err := q.Client().Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer q.Client().Stop(ctx)

	d := dispatch.New(l, q, func(ctx context.Context, owner, name string) (policy.Rules, error) {
		return policy.Rules{Enabled: true, AllowedLabels: []string{"kind/upgrade"}, ActorAllowlist: []string{"shambu2k"}}, nil
	})

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
	})
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
