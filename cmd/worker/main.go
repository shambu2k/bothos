// Command worker dequeues run jobs and processes them. For upgrade runs it
// drives the Phase 2 pipeline: sandbox (work branch off the default), PI agent
// runtime (LLM bump + migration + tests), local commit, then the executor
// pushes and opens the draft PR. It holds no credentials itself — the executor
// resolves the write PAT, and the agent never sees one.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/riverqueue/river"
	"github.com/shambu2k/bothos/internal/credstore"
	"github.com/shambu2k/bothos/internal/executor"
	"github.com/shambu2k/bothos/internal/intent"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/queue"
	"github.com/shambu2k/bothos/internal/runtime"
	"github.com/shambu2k/bothos/internal/runpipe"
	"github.com/shambu2k/bothos/internal/scanjob"
	"github.com/shambu2k/bothos/internal/upgrade"
)

func main() {
	var (
		dsn       = flag.String("dsn", envOr("DATABASE_URL", ""), "Postgres DSN")
		queueName = flag.String("queue", "default", "river queue to consume")
		piAdapter = flag.String("pi-adapter", envOr("PI_ADAPTER", "/usr/local/bin/bothos-pi-adapter"), "path to the PI node adapter")
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

	credStore := credstore.NewEnv(os.Getenv)
	gh := executor.NewGitHubWriter(nil)

	var handler queue.RunHandler = func(ctx context.Context, runID string) error {
		run, err := l.RunByID(ctx, runID)
		if err != nil {
			return err
		}
		if run.Trigger != "upgrade" {
			// Non-upgrade runs keep the Phase 0 no-op behaviour.
			return l.SetRunStatus(ctx, runID, ledger.RunSucceeded)
		}

		var g intent.Grant
		if err := json.Unmarshal(run.Grant, &g); err != nil {
			return failRun(ctx, l, runID, err)
		}

		agent, err := runtime.New("pi", ctx, map[string]any{"adapter": *piAdapter})
		if err != nil {
			return failRun(ctx, l, runID, err)
		}

		pipeline := &runpipe.Pipeline{
			Store:   l,
			Agent:   agent,
			Exec:    executor.NewExecutor(credStore, l, gh, upgrade.GitDiff{Base: g.Scope.BaseRef}, time.Now),
			Sandbox: newSandboxer(),
			ReTest:  retest,
			Commit:  commitWorktree,
		}
		if _, err := pipeline.Run(ctx, runID); err != nil {
			return failRun(ctx, l, runID, err)
		}
		return nil
	}

	q, err := queue.Open(ctx, *dsn, map[string]river.QueueConfig{
		*queueName: {MaxWorkers: 4},
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

// newSandboxer clones the repo's default branch and checks out the work branch
// in an ephemeral temp dir, returning a sandbox bound to that worktree.
func newSandboxer() func(ctx context.Context, repo, branch, baseRef string) (runtime.Sandbox, error) {
	return func(ctx context.Context, repo, branch, baseRef string) (runtime.Sandbox, error) {
		dir, err := os.MkdirTemp("", "bothos-sandbox-")
		if err != nil {
			return nil, err
		}
		wt := dir + "/repo"
		if err := scanjob.ShallowClone(ctx, wt, repo); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		if err := git(ctx, wt, "checkout", "-b", branch); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		return &worksandbox{worktree: wt}, nil
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

// retest re-runs the test command after the agent as the gate before any PR.
func retest(ctx context.Context, worktree, cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = worktree
	if _, err := c.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

// commitWorktree stages and commits the agent's changes locally (no credential
// needed); the executor pushes later.
func commitWorktree(ctx context.Context, worktree, branch string) error {
	if err := git(ctx, worktree, "add", "-A"); err != nil {
		return err
	}
	c := exec.CommandContext(ctx, "git", "-C", worktree, "-c", "user.name=bothos", "-c", "user.email=bothos@localhost", "commit", "-m", "chore(deps): apply upgrade")
	return c.Run()
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
