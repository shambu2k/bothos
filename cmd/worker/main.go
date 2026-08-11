// Command worker dequeues run jobs and processes them. For upgrade runs it
// drives the Phase 2 pipeline: sandbox (work branch off the default), PI agent
// runtime (LLM bump + migration + tests), local commit, then the executor
// pushes and opens the draft PR. It holds no credentials itself — the executor
// resolves the write PAT, and the agent never sees one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/riverqueue/river"
	// Blank import registers the "pi" runtime (agent.init) — without it
	// runtime.New("pi", ...) fails on every upgrade run.
	_ "github.com/shambu2k/bothos/internal/agent"
	"github.com/shambu2k/bothos/internal/credstore"
	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/queue"
	"github.com/shambu2k/bothos/internal/runpipe"
	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/scanjob"
	"github.com/shambu2k/bothos/internal/upgrade"
)

func main() {
	var (
		dsn         = flag.String("dsn", envOr("DATABASE_URL", ""), "Postgres DSN")
		queueName   = flag.String("queue", "default", "river queue to consume")
		piBin       = flag.String("pi", envOr("PI_BIN", "pi"), "path to the pi CLI (RPC mode)")
		piModel     = flag.String("pi-model", envOr("PI_MODEL", ""), "provider/id for the PI agent")
		piSession   = flag.String("pi-session-dir", envOr("PI_SESSION_DIR", "/var/lib/bothos/sessions"), "persistent per-run PI session dir")
		concurrency = flag.Int("concurrency", envIntOr("WORKER_CONCURRENCY", 2), "upgrade runs to process in parallel (each JIVA run is a heavy npm install)")
	)
	flag.Parse()
	if *dsn == "" {
		log.Fatal("DATABASE_URL (or -dsn) is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l, err := ledger.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	defer l.Close()
	if err := l.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	credStore := credstore.NewEnv(os.Getenv)
	gh := executor.NewGitHubWriter(nil)

	var handler queue.RunHandler = func(ctx context.Context, runID string) error {
		run, err := l.RunByID(ctx, runID)
		if err != nil {
			log.Printf("run %s: load: %v", runID, err)
			return err
		}
		switch run.Trigger {
		case "upgrade", "webhook_pull_request":
		default:
			err := fmt.Errorf("unsupported run trigger %q", run.Trigger)
			log.Printf("run %s: %v", runID, err)
			_ = failRun(ctx, l, runID, err)
			return nil
		}

		agent, err := runtime.New("pi", ctx, map[string]any{
			"pi":          *piBin,
			"model":       *piModel,
			"session_dir": *piSession,
			"approve":     true,
		})
		if err != nil {
			log.Printf("run %s: new pi runtime: %v", runID, err)
			_ = failRun(ctx, l, runID, err)
			// Terminal: the run outcome is recorded in the ledger. Returning
			// the error would make River retry the whole expensive agent run
			// (each retry burns up to the full wall cap again).
			return nil
		}

		exec := executor.NewExecutor(credStore, l, gh, upgrade.GitDiff{}, time.Now)
		var runErr error
		switch run.Trigger {
		case "upgrade":
			_, runErr = (&runpipe.Pipeline{
				Store:   l,
				Agent:   agent,
				Exec:    exec,
				Sandbox: newSandboxer(),
			}).Run(ctx, runID)
		case "webhook_pull_request":
			_, runErr = (&runpipe.ReviewPipeline{
				Store:   l,
				Agent:   agent,
				Exec:    exec,
				Sandbox: newReviewSandboxer(),
			}).Run(ctx, runID)
		}
		if runErr != nil {
			log.Printf("run %s: pipeline: %v", runID, runErr)
			_ = failRun(ctx, l, runID, runErr)
			// Terminal: runpipe already recorded status+reason. A River retry
			// would repeat an expensive model run.
			return nil
		}
		return nil
	}

	q, err := queue.Open(ctx, *dsn, map[string]river.QueueConfig{
		*queueName: {MaxWorkers: *concurrency},
	}, handler)
	if err != nil {
		log.Fatalf("queue: %v", err)
	}
	defer q.Close()

	if err := q.Client().Start(ctx); err != nil {
		log.Fatalf("start client: %v", err)
	}
	log.Printf("worker consuming queue %q", *queueName)

	<-ctx.Done()
	log.Println("worker shutting down")
	q.Client().Stop(context.Background())
}

// newSandboxer clones the repo's default branch into an ephemeral temp dir and
// pre-seeds the worktree's git identity so the agent can commit on its own
// bot/<runID>-* branch without -c flags. No branch is created here — the agent
// creates, commits on, and owns its branch; the executor reads it from git
// state.
func newSandboxer() func(ctx context.Context, repo string) (runtime.Sandbox, error) {
	return func(ctx context.Context, repo string) (runtime.Sandbox, error) {
		dir, err := os.MkdirTemp("", "bothos-sandbox-")
		if err != nil {
			return nil, err
		}
		wt := dir + "/repo"
		if err := scanjob.ShallowClone(ctx, wt, repo); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		if err := git(ctx, wt, "config", "user.name", "bothos"); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		if err := git(ctx, wt, "config", "user.email", "bothos@localhost"); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		// Ensure origin/HEAD is resolvable (the base the executor targets).
		_ = git(ctx, wt, "remote", "set-head", "origin", "--auto")
		return &worksandbox{worktree: wt}, nil
	}
}

func newReviewSandboxer() runpipe.ReviewSandboxer {
	return newReviewSandboxerWithURL(func(repo string) string {
		return "https://github.com/" + strings.TrimSuffix(repo, ".git") + ".git"
	})
}

func newReviewSandboxerWithURL(repoURL func(string) string) runpipe.ReviewSandboxer {
	return func(ctx context.Context, repo string, prNumber int, baseSHA, headSHA string) (runtime.Sandbox, error) {
		dir, err := os.MkdirTemp("", "bothos-review-")
		if err != nil {
			return nil, err
		}
		fail := func(cause error) (runtime.Sandbox, error) {
			_ = os.RemoveAll(dir)
			return nil, cause
		}
		worktree := filepath.Join(dir, "repo")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			return fail(err)
		}
		if err := git(ctx, worktree, "init", "-q"); err != nil {
			return fail(fmt.Errorf("init review worktree: %w", err))
		}
		if err := git(ctx, worktree, "remote", "add", "origin", repoURL(repo)); err != nil {
			return fail(fmt.Errorf("add review origin: %w", err))
		}
		if err := git(ctx, worktree, "fetch", "--depth=1", "origin", baseSHA); err != nil {
			return fail(fmt.Errorf("fetch review base: %w", err))
		}
		if err := git(ctx, worktree, "update-ref", "refs/bothos/base", "FETCH_HEAD"); err != nil {
			return fail(fmt.Errorf("record review base: %w", err))
		}
		pullRef := fmt.Sprintf("refs/pull/%d/head", prNumber)
		if err := git(ctx, worktree, "fetch", "--depth=1", "origin", pullRef); err != nil {
			return fail(fmt.Errorf("fetch review head: %w", err))
		}
		if err := git(ctx, worktree, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return fail(fmt.Errorf("checkout review head: %w", err))
		}
		head, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "HEAD").Output()
		if err != nil {
			return fail(fmt.Errorf("resolve review head: %w", err))
		}
		if got := strings.TrimSpace(string(head)); got != headSHA {
			return fail(fmt.Errorf("review head mismatch: fetched %s, granted %s", got, headSHA))
		}
		return &worksandbox{worktree: worktree}, nil
	}
}

type worksandbox struct{ worktree string }

func (s *worksandbox) Worktree() string { return s.worktree }

func (s *worksandbox) Exec(ctx context.Context, cmd string, args ...string) (runtime.Output, error) {
	var out runtime.Output
	c := exec.CommandContext(ctx, cmd, args...)
	c.Dir = s.worktree
	b, err := c.CombinedOutput()
	out.Stdout = string(b)
	if c.ProcessState != nil {
		out.ExitCode = c.ProcessState.ExitCode()
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

func git(ctx context.Context, worktree string, args ...string) error {
	full := append([]string{"-C", worktree}, args...)
	c := exec.CommandContext(ctx, "git", full...)
	if _, err := c.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

func failRun(ctx context.Context, l *ledger.Postgres, runID string, cause error) error {
	_ = l.SetRunStatus(ctx, runID, ledger.RunFailed)
	return cause
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envIntOr returns the int value of env var k, or def if unset/invalid.
func envIntOr(k string, def int) int {
	if s := os.Getenv(k); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return def
}
