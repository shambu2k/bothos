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
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/shambu2k/bothos/internal/ledger"
	"github.com/shambu2k/bothos/internal/policy"
	"github.com/shambu2k/bothos/internal/queue"
)

// RulesLoader resolves per-repo policy rules. Phase 0 falls back to defaults;
// production reads repo_config.
type RulesLoader func(ctx context.Context, owner, name string) (policy.Rules, error)

type ActorAuthorizer func(ctx context.Context, owner, name, actor string) (bool, error)
type PullRequestLoader func(ctx context.Context, owner, name string, number int) (baseSHA, headSHA string, err error)

type Dispatcher struct {
	ledger    *ledger.Postgres
	queue     *queue.Queue
	rules     RulesLoader
	authorize ActorAuthorizer
	loadPR    PullRequestLoader
	now       func() time.Time
	newRun    func() string
}

func New(l *ledger.Postgres, q *queue.Queue, rules RulesLoader, authorize ActorAuthorizer, loadPR PullRequestLoader) *Dispatcher {
	return &Dispatcher{
		ledger:    l,
		queue:     q,
		rules:     rules,
		authorize: authorize,
		loadPR:    loadPR,
		now:       time.Now,
		newRun:    newRunID,
	}
}

// HandleEvent records the policy decision for a webhook event and, if allowed,
// enqueues the run atomically. It returns nil for events this bot doesn't
// dispatch on (ping, unlabeled, ...) after doing nothing.
func (d *Dispatcher) HandleEvent(ctx context.Context, event any) error {
	trig, handled, err := d.triggerFromEvent(ctx, event)
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

var reviewMention = regexp.MustCompile(`(?i)^\s*@bothos\s+review\s*$`)

// triggerFromEvent maps a go-github event to a policy.Trigger, reporting
// handled=false for events the bot doesn't act on.
func (d *Dispatcher) triggerFromEvent(ctx context.Context, event any) (policy.Trigger, bool, error) {
	switch e := event.(type) {
	case *github.PullRequestEvent:
		if e.Repo == nil || e.Repo.Owner == nil || e.Repo.Name == nil || e.PullRequest == nil {
			return policy.Trigger{}, false, nil
		}
		manual := false
		switch e.GetAction() {
		case "opened", "synchronize", "reopened":
		case "labeled":
			if e.Label == nil || e.Label.GetName() != "bothos/review" {
				return policy.Trigger{}, false, nil
			}
			manual = true
		default:
			return policy.Trigger{}, false, nil
		}

		owner, name := e.Repo.Owner.GetLogin(), e.Repo.GetName()
		actor := eventSender(e.Sender)
		canWrite := false
		if manual {
			canWrite = d.actorCanWrite(ctx, owner, name, actor)
		}
		var baseRef, baseSHA, headSHA string
		if e.PullRequest.Base != nil {
			baseRef = e.PullRequest.Base.GetRef()
			baseSHA = e.PullRequest.Base.GetSHA()
		}
		if e.PullRequest.Head != nil {
			headSHA = e.PullRequest.Head.GetSHA()
		}
		number := e.GetNumber()
		if number == 0 {
			number = int(e.PullRequest.GetNumber())
		}
		return policy.Trigger{
			Kind:          policy.TriggerPullRequest,
			Owner:         owner,
			Name:          name,
			Number:        number,
			BaseRef:       baseRef,
			BaseSHA:       baseSHA,
			HeadSHA:       headSHA,
			Actor:         actor,
			ActorHasWrite: canWrite,
			Manual:        manual,
		}, true, nil

	case *github.IssueCommentEvent:
		if e.GetAction() != "created" || e.Repo == nil || e.Repo.Owner == nil ||
			e.Repo.Name == nil || e.Issue == nil || e.Comment == nil ||
			e.Issue.PullRequestLinks == nil || e.Sender == nil ||
			strings.EqualFold(e.Sender.GetType(), "Bot") {
			return policy.Trigger{}, false, nil
		}
		matched := false
		for _, line := range strings.Split(e.Comment.GetBody(), "\n") {
			if reviewMention.MatchString(line) {
				matched = true
				break
			}
		}
		if !matched {
			return policy.Trigger{}, false, nil
		}

		owner, name := e.Repo.Owner.GetLogin(), e.Repo.GetName()
		actor := eventSender(e.Sender)
		canWrite := d.actorCanWrite(ctx, owner, name, actor)
		trigger := policy.Trigger{
			Kind:          policy.TriggerPullRequest,
			Owner:         owner,
			Name:          name,
			Number:        int(e.Issue.GetNumber()),
			Actor:         actor,
			ActorHasWrite: canWrite,
			Manual:        true,
		}
		if !canWrite {
			return trigger, true, nil
		}
		if d.loadPR == nil {
			return policy.Trigger{}, false, fmt.Errorf("load pull request: loader not configured")
		}
		baseSHA, headSHA, err := d.loadPR(ctx, owner, name, trigger.Number)
		if err != nil {
			return policy.Trigger{}, false, fmt.Errorf("load pull request: %w", err)
		}
		trigger.BaseSHA, trigger.HeadSHA = baseSHA, headSHA
		return trigger, true, nil

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
			ActorHasWrite: false,
			LabelsApplied: []string{e.Label.GetName()},
		}, true, nil

	default:
		return policy.Trigger{}, false, nil
	}
}

func (d *Dispatcher) actorCanWrite(ctx context.Context, owner, name, actor string) bool {
	if d.authorize == nil {
		return false
	}
	allowed, err := d.authorize(ctx, owner, name, actor)
	if err != nil {
		log.Printf("authorize %s/%s actor %q: %v", owner, name, actor, err)
		return false
	}
	return allowed
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
