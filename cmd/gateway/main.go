// Command gateway validates GitHub webhooks, computes dispatch-time policy,
// records runs, and enqueues allowed work. Its GitHub read token is limited to
// collaborator permission and pull-request metadata lookups.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/dispatch"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/logx"
	"github.com/shambu2k/bothos/internal/queue"
)

// logger is the process-wide structured logger. webhookHandler is exercised
// directly by tests, so it reads the shared logger here instead of taking one
// as an argument.
var logger = logx.New()

func main() {
	var (
		addr       = flag.String("addr", ":8090", "listen address (tunnel target)")
		dsn        = flag.String("dsn", envOr("DATABASE_URL", "postgres://maintbot:maintbot-dev@localhost:5432/maintbot"), "Postgres DSN")
		webhookKey = flag.String("webhook-secret", os.Getenv("GITHUB_WEBHOOK_SECRET"), "GitHub webhook secret")
	)
	flag.Parse()

	if *webhookKey == "" {
		logger.Error("GITHUB_WEBHOOK_SECRET is required")
		os.Exit(1)
	}

	ctx := context.Background()

	l, err := ledger.New(ctx, *dsn)
	if err != nil {
		logger.Error("ledger init failed", "err", err)
		os.Exit(1)
	}
	defer l.Close()
	if err := l.Migrate(ctx); err != nil {
		logger.Error("ledger migrate failed", "err", err)
		os.Exit(1)
	}

	q, err := queue.Open(ctx, *dsn, nil, nil)
	if err != nil {
		logger.Error("queue open failed", "err", err)
		os.Exit(1)
	}
	defer q.Close()

	readToken := os.Getenv("GITHUB_READ_TOKEN")
	client := github.NewClient(nil)
	if readToken != "" {
		client = client.WithAuthToken(readToken)
	}
	d := dispatch.New(l, q, l.RulesForRepo, newActorAuthorizer(client, readToken != ""), newPullRequestLoader(client))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", webhookHandler(*webhookKey, d))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	logger.Info("gateway listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, requestLogger(mux)); err != nil {
		logger.Error("http server failed", "err", err)
		os.Exit(1)
	}
}

// requestLogger wraps h and emits one structured line per request carrying
// method, path, HTTP status, and duration in milliseconds.
func requestLogger(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(sr, r)
		if sr.status == 0 {
			sr.status = http.StatusOK
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusRecorder captures the response status code written by an inner
// handler while forwarding Header/Write/WriteHeader to the real writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func newActorAuthorizer(client *github.Client, tokenConfigured bool) dispatch.ActorAuthorizer {
	return func(ctx context.Context, owner, name, actor string) (bool, error) {
		if !tokenConfigured {
			return false, nil
		}
		permission, _, err := client.Repositories.GetPermissionLevel(ctx, owner, name, actor)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(permission.GetPermission()) {
		case "admin", "maintain", "write":
			return true, nil
		default:
			return false, nil
		}
	}
}

func newPullRequestLoader(client *github.Client) dispatch.PullRequestLoader {
	return func(ctx context.Context, owner, name string, number int) (string, string, error) {
		pullRequest, _, err := client.PullRequests.Get(ctx, owner, name, number)
		if err != nil {
			return "", "", err
		}
		baseSHA, headSHA := pullRequest.GetBase().GetSHA(), pullRequest.GetHead().GetSHA()
		if baseSHA == "" || headSHA == "" {
			return "", "", fmt.Errorf("pull request %s/%s#%d has empty base/head SHA", owner, name, number)
		}
		return baseSHA, headSHA, nil
	}
}

func webhookHandler(secret string, d *dispatch.Dispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, err := github.ValidatePayload(r, []byte(secret))
		if err != nil {
			logger.Error("signature validation failed", "event", github.WebHookType(r), "err", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		event, err := github.ParseWebHook(github.WebHookType(r), payload)
		if err != nil {
			logger.Error("parse webhook failed", "event", github.WebHookType(r), "err", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if err := d.HandleEvent(r.Context(), event); err != nil {
			logger.Error("dispatch failed", "event", fmt.Sprintf("%T", event), "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		logger.Info("webhook handled", "event", github.WebHookType(r), "type", fmt.Sprintf("%T", event))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
