// Package dispatch is the gateway's decision logic: it turns a GitHub webhook
// event into a run row plus (if allowed) an enqueued job. It is free of HTTP —
// the gateway handler only validates the signature and calls HandleEvent — so
// it is unit-testable against a real Postgres without network.
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/maintainer-bot/internal/ledger"
	"github.com/shambu2k/maintainer-bot/internal/policy"
	"github.com/shambu2k/maintainer-bot/internal/queue"
)

// RulesLoader resolves per-repo policy rules. Phase 0 falls back to defaults;
// production reads repo_config.
type RulesLoader func(ctx context.Context, owner, name string) (policy.Rules, error)

type Dispatcher struct {
	ledger *ledger.Postgres
	queue  *queue.Queue
	rules  RulesLoader
	now    func() time.Time
	newRun func() string
}

func New(l *ledger.Postgres, q *queue.Queue, rules RulesLoader) *Dispatcher {
	return &Dispatcher{
		ledger: l,
		queue:  q,
		rules:  rules,
		now:    time.Now,
		newRun: newRunID,
	}
}

// HandleEvent records the policy decision for a webhook event and, if allowed,
// enqueues the run atomically. It returns nil for events this bot doesn't
// dispatch on (ping, unlabeled, ...) after doing nothing.
func (d *Dispatcher) HandleEvent(ctx context.Context, event any) error {
	trig, handled, err := d.triggerFromEvent(event)
	if err != nil {
		return err
	}
	if !handled {
		return nil
	}

	rules, err := d.rules(ctx, trig.Owner, trig.Name)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}

	runID := d.newRun()
	g, decideErr := policy.Decide(trig, rules, runID, d.now())

	if decideErr != nil {
		// A denied run is still recorded — with nothing else happening. That
		// is the Phase 4 failure mode: decision in the ledger, no side effect.
		grantJSON, _ := json.Marshal(g)
		return d.ledger.InsertRun(ctx, ledger.Run{
			ID:          runID,
			RepoID:      trig.Owner + "/" + trig.Name,
			Trigger:     trig.Kind.String(),
			ScopeKind:   string(g.Scope.Kind),
			ScopeNumber: g.Scope.Number,
			Grant:       grantJSON,
			Decision:    "deny",
			DenyReason:  decideErr.Error(),
			Status:      ledger.RunDenied,
		})
	}

	grantJSON, _ := json.Marshal(g)
	run := ledger.Run{
		ID:          runID,
		RepoID:      trig.Owner + "/" + trig.Name,
		Trigger:     trig.Kind.String(),
		ScopeKind:   string(g.Scope.Kind),
		ScopeNumber: g.Scope.Number,
		Grant:       grantJSON,
		Decision:    "allow",
		Status:      ledger.RunQueued,
	}

	// The run row and its queue job commit atomically, so a redelivered or
	// retried webhook cannot orphan a job.
	tx, err := d.queue.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := d.ledger.InsertRunTx(ctx, tx, run); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	if err := d.queue.EnqueueTx(ctx, tx, runID); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// triggerFromEvent maps a go-github event to a policy.Trigger, reporting
// handled=false for events the bot doesn't act on.
func (d *Dispatcher) triggerFromEvent(event any) (policy.Trigger, bool, error) {
	switch e := event.(type) {
	case *github.PullRequestEvent:
		if e.Repo == nil || e.Repo.Owner == nil || e.Repo.Name == nil || e.PullRequest == nil {
			return policy.Trigger{}, false, nil
		}
		if e.GetAction() != "opened" && e.GetAction() != "synchronize" && e.GetAction() != "reopened" {
			return policy.Trigger{}, false, nil
		}
		var base, head string
		if e.PullRequest.Base != nil {
			base = e.PullRequest.Base.GetRef()
		}
		if e.PullRequest.Head != nil {
			head = e.PullRequest.Head.GetSHA()
		}
		return policy.Trigger{
			Kind:  policy.TriggerPullRequest,
			Owner: e.Repo.Owner.GetLogin(),
			Name:  e.Repo.GetName(),
			Number: func() int {
				if e.Number != nil {
					return e.GetNumber()
				}
				return int(e.PullRequest.GetNumber())
			}(),
			BaseRef: base,
			HeadSHA: head,
		}, true, nil

	case *github.IssuesEvent:
		if e.Repo == nil || e.Repo.Owner == nil || e.Repo.Name == nil || e.Issue == nil || e.Label == nil {
			return policy.Trigger{}, false, nil
		}
		if e.GetAction() != "labeled" {
			return policy.Trigger{}, false, nil
		}
		return policy.Trigger{
			Kind:          policy.TriggerIssueLabeled,
			Owner:         e.Repo.Owner.GetLogin(),
			Name:          e.Repo.GetName(),
			Number:        int(e.Issue.GetNumber()),
			DefaultBranch: e.Repo.GetDefaultBranch(),
			Actor:         eventSender(e.Sender),
			ActorHasWrite: false, // resolved from perms/TripleA in a later phase
			LabelsApplied: []string{e.Label.GetName()},
		}, true, nil

	default:
		return policy.Trigger{}, false, nil
	}
}

func eventSender(u *github.User) string {
	if u == nil {
		return ""
	}
	return u.GetLogin()
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
