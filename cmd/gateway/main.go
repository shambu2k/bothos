// Command gateway validates GitHub webhooks, computes dispatch-time policy,
// records runs, and enqueues allowed work. Its GitHub read token is limited to
// collaborator permission and pull-request metadata lookups.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/dispatch"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/policy"
	"github.com/shambu2k/bothos/internal/queue"
)

func main() {
	var (
		addr       = flag.String("addr", ":8090", "listen address (tunnel target)")
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

	readToken := os.Getenv("GITHUB_READ_TOKEN")
	client := github.NewClient(nil)
	if readToken != "" {
		client = client.WithAuthToken(readToken)
	}
	d := dispatch.New(l, q, defaultRules, newActorAuthorizer(client, readToken != ""), newPullRequestLoader(client))

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
		log.Printf("webhook %s (%T)", github.WebHookType(r), event)
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
