package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/store"
	"github.com/virajchogle/anchor/internal/verify"
)

// Embedder turns text into an embedding. Declared here, where it is consumed,
// so the protocol package does not depend on Bedrock.
type Embedder interface {
	Embed(ctx context.Context, text string) (store.Vector, error)
}

// Reconciler resolves intents abandoned by processes that died mid-action.
//
// This is the component that makes the exactly-once claim true rather than
// aspirational. Retrying an external call gets you at-least-once. What gets you
// exactly-once is that every abandoned intent is settled by asking the external
// system what actually happened, and that the answer commits in the same
// transaction as the memory describing it.
//
// The reconciler never re-executes an action. It only observes and records.
type Reconciler struct {
	db       *pgxpool.Pool
	registry *verify.Registry
	coord    *Coordinator
	embed    Embedder
	agentID  uuid.UUID
	leaseTTL time.Duration
	batch    int
	log      *slog.Logger
}

func NewReconciler(db *pgxpool.Pool, reg *verify.Registry, coord *Coordinator, emb Embedder,
	agentID uuid.UUID, leaseTTL time.Duration, log *slog.Logger) *Reconciler {
	if leaseTTL <= 0 {
		leaseTTL = 2 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{
		db: db, registry: reg, coord: coord, embed: emb,
		agentID: agentID, leaseTTL: leaseTTL, batch: 32, log: log,
	}
}

// Stats summarizes one reconciliation pass.
type Stats struct {
	Claimed   int
	Committed int
	Failed    int
	Escalated int
}

// claimExpired atomically takes ownership of intents whose lease has lapsed.
//
// SKIP LOCKED is what makes running several reconcilers safe: a row already
// locked by a peer is passed over rather than blocking this scan. Without it,
// two reconcilers serialize behind each other and a single slow verification
// stalls every other recovery.
//
// Taking the lease also bumps attempts, so an intent that repeatedly fails to
// verify is visible as such rather than silently spinning.
const claimExpired = `
WITH claimable AS (
  SELECT idem_key
    FROM action_intents
   WHERE state = 'PENDING' AND lease_expires < now()
   ORDER BY lease_expires
   LIMIT $3
   FOR UPDATE SKIP LOCKED
)
UPDATE action_intents ai
   SET lease_owner = $1,
       lease_expires = now() + $2::INTERVAL,
       attempts = attempts + 1
  FROM claimable c
 WHERE ai.idem_key = c.idem_key
RETURNING ai.idem_key, ai.episode_id, ai.agent_id, ai.action_type, ai.args,
          ai.state, ai.lease_owner, ai.lease_expires,
          coalesce(ai.external_ref, ''), ai.outcome, ai.attempts`

func (r *Reconciler) claimExpired(ctx context.Context) ([]Intent, error) {
	rows, err := r.db.Query(ctx, claimExpired,
		r.agentID, fmt.Sprintf("%d seconds", int(r.leaseTTL.Seconds())), r.batch)
	if err != nil {
		return nil, fmt.Errorf("claiming expired intents: %w", err)
	}
	defer rows.Close()

	var out []Intent
	for rows.Next() {
		var in Intent
		if err := rows.Scan(&in.IdemKey, &in.EpisodeID, &in.AgentID, &in.ActionType,
			&in.Args, &in.State, &in.LeaseOwner, &in.LeaseExpires,
			&in.ExternalRef, &in.Outcome, &in.Attempts); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RunOnce performs a single reconciliation pass.
func (r *Reconciler) RunOnce(ctx context.Context) (Stats, error) {
	claimed, err := r.claimExpired(ctx)
	if err != nil {
		return Stats{}, err
	}

	st := Stats{Claimed: len(claimed)}
	for _, in := range claimed {
		if err := r.reconcileOne(ctx, in); err != nil {
			r.log.Error("reconcile failed",
				"idem_key", in.IdemKey, "action_type", in.ActionType, "error", err)
		}
	}

	// Recount from the database rather than from in-memory bookkeeping, so the
	// numbers reflect committed state instead of intended state.
	for _, in := range claimed {
		cur, err := r.coord.Load(ctx, in.IdemKey)
		if err != nil {
			continue
		}
		switch cur.State {
		case StateCommitted:
			st.Committed++
		case StateFailed:
			st.Failed++
		default:
			st.Escalated++
		}
	}
	return st, nil
}

// reconcileOne settles a single abandoned intent against external ground truth.
func (r *Reconciler) reconcileOne(ctx context.Context, in Intent) error {
	verdict, err := r.registry.Verify(ctx, in.ActionType, in.Args, in.IdemKey, in.ExternalRef)
	if err != nil {
		// A verifier that errors tells us nothing about the world. Treat it
		// exactly like Unknown: back off and leave the intent for a human or a
		// later pass. Never assume.
		return r.escalate(ctx, in, fmt.Sprintf("verifier error: %v", err))
	}

	switch verdict.Disposition {
	case verify.Applied:
		return r.commitRecovered(ctx, in, verdict)

	case verify.NotApplied:
		// The external system confirms nothing happened. Marking FAILED rather
		// than re-executing is deliberate: the brief's rule is that a failed
		// action surfaces to an operator instead of being silently retried.
		return r.markFailed(ctx, in, verdict.Reason)

	default:
		return r.escalate(ctx, in, verdict.Reason)
	}
}

// commitRecovered writes the memory of an action that a dead process performed.
//
// The narrative deliberately records that recovery happened. That is not
// bookkeeping noise: it is the truthful account of the incident, and a future
// recall over this scope should surface that this class of action has crashed
// mid-flight before.
func (r *Reconciler) commitRecovered(ctx context.Context, in Intent, v verify.Verdict) error {
	effect, err := r.registry.Effect(in.ActionType, in.Args)
	if err != nil {
		return fmt.Errorf("reconstructing world effect: %w", err)
	}

	externalRef := v.ExternalRef
	if externalRef == "" {
		externalRef = in.ExternalRef
	}

	narrative := fmt.Sprintf(
		"Action %s was verified as applied after the originating process failed before recording it. "+
			"Recovered by reconciler on attempt %d. Evidence: %s",
		in.ActionType, in.Attempts, v.Reason)

	emb, err := r.embed.Embed(ctx, narrative)
	if err != nil {
		return fmt.Errorf("embedding recovery narrative: %w", err)
	}

	err = r.coord.CommitAtomic(ctx, Commit{
		IdemKey: in.IdemKey,
		Receipt: &verify.Receipt{ExternalRef: externalRef, Outcome: v.Outcome},
		Cluster: effect,
		Memory: MemoryWrite{
			EpisodeID: in.EpisodeID,
			Narrative: narrative,
			Outcome:   "recovered",
			Embedding: emb,
		},
	})
	if err != nil {
		return err
	}

	r.log.Info("reconciled orphaned intent",
		"idem_key", in.IdemKey, "action_type", in.ActionType,
		"disposition", verify.Applied.String(), "external_ref", externalRef,
		"attempts", in.Attempts)
	return nil
}

const markFailedSQL = `
UPDATE action_intents
   SET state = 'FAILED', outcome = $2, resolved_at = now(), lease_owner = NULL
 WHERE idem_key = $1 AND state = 'PENDING'`

func (r *Reconciler) markFailed(ctx context.Context, in Intent, reason string) error {
	payload, _ := json.Marshal(map[string]string{
		"disposition": verify.NotApplied.String(),
		"reason":      reason,
	})
	tag, err := r.db.Exec(ctx, markFailedSQL, in.IdemKey, payload)
	if err != nil {
		return fmt.Errorf("marking intent failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // someone else resolved it first
	}
	r.log.Warn("intent verified as not applied, surfacing to operator",
		"idem_key", in.IdemKey, "action_type", in.ActionType, "reason", reason)
	return nil
}

// escalate handles the Unknown case: we could not establish ground truth.
//
// The intent deliberately stays PENDING. Auto-resolving it either way would be
// the exact failure this project exists to prevent, so instead the lease is
// pushed out by a backoff and the reason is recorded for an operator. It will be
// retried on a later pass in case the external system becomes readable again.
const escalateSQL = `
UPDATE action_intents
   SET lease_expires = now() + $2::INTERVAL, outcome = $3, lease_owner = NULL
 WHERE idem_key = $1 AND state = 'PENDING'`

func (r *Reconciler) escalate(ctx context.Context, in Intent, reason string) error {
	payload, _ := json.Marshal(map[string]any{
		"disposition":       verify.Unknown.String(),
		"reason":            reason,
		"attempts":          in.Attempts,
		"needs_operator":    in.Attempts >= 3,
		"last_escalated_at": time.Now().UTC().Format(time.RFC3339),
	})

	backoff := store.Backoff(in.Attempts) + 30*time.Second
	if _, err := r.db.Exec(ctx, escalateSQL, in.IdemKey,
		fmt.Sprintf("%d seconds", int(backoff.Seconds())), payload); err != nil {
		return fmt.Errorf("escalating intent: %w", err)
	}

	level := slog.LevelWarn
	if in.Attempts >= 3 {
		level = slog.LevelError
	}
	r.log.Log(ctx, level, "intent could not be verified, left pending for operator",
		"idem_key", in.IdemKey, "action_type", in.ActionType,
		"attempts", in.Attempts, "reason", reason)
	return nil
}

// Run reconciles on startup and then on a timer until the context is cancelled.
//
// The startup pass matters as much as the timer: a process that crashed
// mid-action leaves an intent that nothing else will notice, so recovery has to
// begin the moment an agent comes back up.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	pass := func() {
		st, err := r.RunOnce(ctx)
		if err != nil {
			r.log.Error("reconciliation pass failed", "error", err)
			return
		}
		if st.Claimed > 0 {
			r.log.Info("reconciliation pass complete",
				"claimed", st.Claimed, "committed", st.Committed,
				"failed", st.Failed, "escalated", st.Escalated)
		}
	}

	pass()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			pass()
		}
	}
}

// PendingOlderThan lists unresolved intents for the observability panel.
const pendingOlderThan = `
SELECT idem_key, episode_id, agent_id, action_type, args, state,
       lease_owner, lease_expires, coalesce(external_ref, ''), outcome, attempts
  FROM action_intents
 WHERE state = 'PENDING' AND created_at < now() - $1::INTERVAL
 ORDER BY created_at`

func (r *Reconciler) PendingOlderThan(ctx context.Context, age time.Duration) ([]Intent, error) {
	rows, err := r.db.Query(ctx, pendingOlderThan, fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Intent
	for rows.Next() {
		var in Intent
		if err := rows.Scan(&in.IdemKey, &in.EpisodeID, &in.AgentID, &in.ActionType,
			&in.Args, &in.State, &in.LeaseOwner, &in.LeaseExpires,
			&in.ExternalRef, &in.Outcome, &in.Attempts); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
