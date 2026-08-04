// Command gateway is the webhook receiver. It validates the GitHub signature,
// computes the dispatch-time policy decision, records the run in the ledger,
// and enqueues the job atomically. It holds the webhook secret but never a
// GitHub token.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/maintainer-bot/internal/dispatch"
	"github.com/shambu2k/maintainer-bot/internal/ledger"
	"github.com/shambu2k/maintainer-bot/internal/policy"
	"github.com/shambu2k/maintainer-bot/internal/queue"
)

func main() {
	var (
		addr       = flag.String("addr", ":8080", "listen address")
		dsn        = flag.String("dsn", envOr("DATABASE_URL", "postgres://maintbot:maintbot-dev@localhost:5432/maintbot"), "Postgres DSN")
		webhookKey = flag.String("webhook-secret", os.Getenv("GITHUB_WEBHOOK_SECRET"), "GitHub webhook secret")
	)
	flag.Parse()

	if *webhookKey == "" {
		log.Fatal("GITHUB_WEBHOOK_SECRET is required")
	}

	ctx := context.Background()

	l, err := ledger.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	defer l.Close()
	if err := l.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	q, err := queue.Open(ctx, *dsn, nil, nil)
	if err != nil {
		log.Fatalf("queue: %v", err)
	}
	defer q.Close()

	d := dispatch.New(l, q, defaultRules)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", webhookHandler(*webhookKey, d))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("gateway listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// defaultRules is Phase 0's static policy; production reads repo_config.
func defaultRules(ctx context.Context, owner, name string) (policy.Rules, error) {
	return policy.Rules{
		Enabled:        true,
		AllowedLabels:  []string{"kind/upgrade"},
		ActorAllowlist: []string{"shambu2k"},
	}, nil
}

func webhookHandler(secret string, d *dispatch.Dispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, err := github.ValidatePayload(r, []byte(secret))
		if err != nil {
			log.Printf("signature validation failed: %v", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		event, err := github.ParseWebHook(github.WebHookType(r), payload)
		if err != nil {
			log.Printf("parse webhook: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if err := d.HandleEvent(r.Context(), event); err != nil {
			log.Printf("dispatch %T: %v", event, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
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
