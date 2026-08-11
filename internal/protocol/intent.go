package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/store"
	"github.com/virajchogle/anchor/internal/verify"
)

// State values for action_intents.state. These are persisted, so the strings are
// part of the on-disk contract and must not be renamed casually.
const (
	StatePending   = "PENDING"
	StateCommitted = "COMMITTED"
	StateFailed    = "FAILED"
)

// Disposition is the result of phase 1. It tells the caller whether it may
// proceed to phase 2, and if not, why not.
type Disposition int

const (
	// Owned means this process inserted the intent and holds a live lease. It is
	// the only disposition that permits executing the external call.
	Owned Disposition = iota

	// AlreadyCommitted means an identical action already completed and was
	// verified. The recorded outcome is returned and nothing is executed. This
	// is the deduplication path that makes a retried request safe.
	AlreadyCommitted

	// Busy means another agent holds a live lease on this exact action. The
	// caller backs off and re-reads rather than racing it.
	Busy

	// Orphaned means a PENDING intent exists whose lease has expired: some
	// process began this action and died. The external world may or may not have
	// changed. Only the reconciler may resolve it, because resolving it requires
	// querying the external system for ground truth.
	Orphaned

	// Failed means a previous attempt was verified as not applied, or could not
	// be resolved. It is surfaced to an operator and never silently retried.
	Failed
)

func (d Disposition) String() string {
	switch d {
	case Owned:
		return "OWNED"
	case AlreadyCommitted:
		return "ALREADY_COMMITTED"
	case Busy:
		return "BUSY"
	case Orphaned:
		return "ORPHANED"
	case Failed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// Intent is a row of action_intents.
type Intent struct {
	IdemKey      string
	EpisodeID    uuid.UUID
	AgentID      uuid.UUID
	ActionType   string
	Args         json.RawMessage
	State        string
	LeaseOwner   *uuid.UUID
	LeaseExpires *time.Time
	ExternalRef  string
	Outcome      json.RawMessage
	Attempts     int
}

// Coordinator runs the three-phase exactly-once protocol.
type Coordinator struct {
	db       *pgxpool.Pool
	registry *verify.Registry
	agentID  uuid.UUID
	// leaseTTL bounds how long a crashed process can block an action before the
	// reconciler is allowed to claim it. Too short and a slow-but-alive agent
	// gets its work stolen; too long and recovery from a real crash stalls.
	leaseTTL time.Duration
}

func NewCoordinator(db *pgxpool.Pool, reg *verify.Registry, agentID uuid.UUID, leaseTTL time.Duration) *Coordinator {
	if leaseTTL <= 0 {
		leaseTTL = 2 * time.Minute
	}
	return &Coordinator{db: db, registry: reg, agentID: agentID, leaseTTL: leaseTTL}
}

const insertIntent = `
INSERT INTO action_intents (idem_key, episode_id, agent_id, action_type, args,
                            state, lease_owner, lease_expires, attempts)
VALUES ($1, $2, $3, $4, $5, 'PENDING', $3, now() + $6::INTERVAL, 1)
ON CONFLICT (idem_key) DO NOTHING
RETURNING idem_key`

const selectIntent = `
SELECT idem_key, episode_id, agent_id, action_type, args, state,
       lease_owner, lease_expires, coalesce(external_ref, ''), outcome, attempts
  FROM action_intents
 WHERE idem_key = $1`

// Intend runs phase 1. It records the durable intent to act before any external
// call happens, which is what makes recovery possible at all: a crash after this
// point leaves evidence that the action may be in flight.
//
// The insert is the deduplication primitive. ON CONFLICT DO NOTHING means exactly
// one caller receives a row back; every other caller falls through to reading the
// existing row and branching on its state.
func (c *Coordinator) Intend(ctx context.Context, episodeID uuid.UUID, actionType string, args any) (*Intent, Disposition, error) {
	rawArgs, err := CanonicalJSON(args)
	if err != nil {
		return nil, Failed, fmt.Errorf("phase 1: %w", err)
	}
	idemKey, err := IdemKey(episodeID.String(), actionType, args)
	if err != nil {
		return nil, Failed, fmt.Errorf("phase 1: %w", err)
	}

	var claimed string
	err = c.db.QueryRow(ctx, insertIntent,
		idemKey, episodeID, c.agentID, actionType, rawArgs,
		fmt.Sprintf("%d seconds", int(c.leaseTTL.Seconds())),
	).Scan(&claimed)

	switch {
	case err == nil:
		// We inserted it, so we own the lease and may execute.
		return &Intent{
			IdemKey: idemKey, EpisodeID: episodeID, AgentID: c.agentID,
			ActionType: actionType, Args: rawArgs, State: StatePending, Attempts: 1,
		}, Owned, nil

	case errors.Is(err, pgx.ErrNoRows):
		// Someone else got there first. Read their row and decide.
		return c.inspectExisting(ctx, idemKey)

	default:
		// An ambiguous failure here is benign: phase 1 has no external side
		// effect, so the worst case is an orphaned PENDING row the reconciler
		// will verify as NotApplied and clear.
		return nil, Failed, fmt.Errorf("phase 1: inserting intent (%s): %w", store.Classify(err), err)
	}
}

// inspectExisting branches on the state of an intent another actor already owns.
func (c *Coordinator) inspectExisting(ctx context.Context, idemKey string) (*Intent, Disposition, error) {
	in, err := c.Load(ctx, idemKey)
	if err != nil {
		return nil, Failed, fmt.Errorf("phase 1: reading conflicting intent: %w", err)
	}

	switch in.State {
	case StateCommitted:
		// The action already happened and was verified. Return the recorded
		// outcome. Executing again here is precisely the double-action this
		// protocol exists to prevent.
		return in, AlreadyCommitted, nil

	case StateFailed:
		return in, Failed, nil

	case StatePending:
		if in.LeaseExpires != nil && in.LeaseExpires.After(time.Now()) {
			// Another agent is actively working this action.
			return in, Busy, nil
		}
		// The lease lapsed, so the owning process is presumed dead. We must not
		// simply take over and execute: the dead process may have already made
		// the external call. Only the reconciler, which queries ground truth,
		// may resolve this.
		return in, Orphaned, nil

	default:
		return in, Failed, fmt.Errorf("intent %s has unrecognized state %q", idemKey, in.State)
	}
}

// Load reads a single intent by key.
func (c *Coordinator) Load(ctx context.Context, idemKey string) (*Intent, error) {
	var in Intent
	err := c.db.QueryRow(ctx, selectIntent, idemKey).Scan(
		&in.IdemKey, &in.EpisodeID, &in.AgentID, &in.ActionType, &in.Args,
		&in.State, &in.LeaseOwner, &in.LeaseExpires, &in.ExternalRef,
		&in.Outcome, &in.Attempts,
	)
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// MemoryWrite is the episode mutation that commits alongside the action.
type MemoryWrite struct {
	EpisodeID uuid.UUID
	Narrative string
	Outcome   string
	// store.Vector, not []float32. pgx encodes a bare []float32 as a PostgreSQL
	// array literal, which CockroachDB's VECTOR type rejects. See store.Vector.
	Embedding store.Vector
}

// ClusterEffect is the world-state mutation that commits alongside the action.
// It is optional; not every action changes a tracked cluster.
type ClusterEffect struct {
	ClusterID    string
	DesiredNodes int
	LastAction   string
}

// Commit describes everything phase 3 writes in a single transaction.
type Commit struct {
	IdemKey string
	Receipt *verify.Receipt
	Cluster *ClusterEffect
	Memory  MemoryWrite
}

// CommitAtomic runs phase 3: the intent resolution, the world-state mutation, and
// the memory write with its embedding, in one serializable transaction.
//
// This single transaction is the thesis. If the memory write were a second
// transaction, or lived in a separate vector database, it could fail alone and
// leave the agent's history disagreeing with what it actually did.
//
// Retry semantics are deliberately asymmetric:
//   - 40001 is replayed. CockroachDB aborted the transaction, so nothing took
//     effect, and replaying re-runs SQL only. The external call is NOT repeated
//     on this path, so a replay cannot double-act.
//   - Ambiguous errors are not replayed and not swallowed. They are returned so
//     the caller routes them to the reconciler, which establishes ground truth
//     before resolving anything.
func (c *Coordinator) CommitAtomic(ctx context.Context, cm Commit) error {
	const maxAttempts = 8

	for attempt := 1; ; attempt++ {
		err := c.commitOnce(ctx, cm)
		if err == nil {
			return nil
		}

		switch class := store.Classify(err); class {
		case store.Retryable:
			if attempt >= maxAttempts {
				return fmt.Errorf("phase 3: gave up after %d serialization retries: %w", attempt, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(store.Backoff(attempt)):
			}
			continue

		case store.Ambiguous:
			// Do not retry and do not report failure. We genuinely do not know
			// whether this committed, and the reconciler is the only component
			// allowed to decide.
			return &AmbiguousCommitError{IdemKey: cm.IdemKey, Err: err}

		default:
			return fmt.Errorf("phase 3: %w", err)
		}
	}
}

// AmbiguousCommitError signals that phase 3 may or may not have committed. The
// caller must not retry the action; it must hand the intent to the reconciler.
type AmbiguousCommitError struct {
	IdemKey string
	Err     error
}

func (e *AmbiguousCommitError) Error() string {
	return fmt.Sprintf("phase 3 commit ambiguous for intent %s, routing to reconciler: %v", e.IdemKey, e.Err)
}
func (e *AmbiguousCommitError) Unwrap() error { return e.Err }

const resolveIntent = `
UPDATE action_intents
   SET state = 'COMMITTED', external_ref = $2, outcome = $3,
       resolved_at = now(), lease_owner = NULL
 WHERE idem_key = $1 AND state = 'PENDING'`

const mutateCluster = `
UPDATE managed_clusters
   SET desired_nodes = $2, last_action = $3, version = version + 1
 WHERE cluster_id = $1`

// The episode is UPDATEd rather than INSERTed because it was created at incident
// open so that idem_key had an episode_id to hash. expires_at is set to NULL in
// this same statement: an episode with a committed action is pinned against the
// row-level TTL forever. Memory decays, but the record of what the agent did to
// the world does not.
const writeMemory = `
UPDATE episodes
   SET narrative = $2, outcome = $3, embedding = $4,
       status = 'resolved', expires_at = NULL
 WHERE episode_id = $1`

func (c *Coordinator) commitOnce(ctx context.Context, cm Commit) error {
	tx, err := c.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var outcome json.RawMessage
	var externalRef string
	if cm.Receipt != nil {
		outcome = cm.Receipt.Outcome
		externalRef = cm.Receipt.ExternalRef
	}
	if len(outcome) == 0 {
		// The schema CHECK requires evidence on a COMMITTED row. Encode an
		// explicit empty object rather than letting NULL violate it, so the
		// failure surfaces here as a bug rather than as a constraint error.
		outcome = json.RawMessage(`{}`)
	}

	tag, err := tx.Exec(ctx, resolveIntent, cm.IdemKey, externalRef, outcome)
	if err != nil {
		return fmt.Errorf("resolving intent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The WHERE clause requires state='PENDING'. Zero rows means someone
		// else already resolved it, so committing our version would overwrite a
		// verified outcome. Abort rather than clobber.
		return fmt.Errorf("intent %s was no longer PENDING at commit time", cm.IdemKey)
	}

	if cm.Cluster != nil {
		if _, err := tx.Exec(ctx, mutateCluster,
			cm.Cluster.ClusterID, cm.Cluster.DesiredNodes, cm.Cluster.LastAction); err != nil {
			return fmt.Errorf("mutating cluster state: %w", err)
		}
	}

	if err := cm.Memory.Embedding.Validate(); err != nil {
		return fmt.Errorf("writing memory: %w", err)
	}
	if _, err := tx.Exec(ctx, writeMemory,
		cm.Memory.EpisodeID, cm.Memory.Narrative, cm.Memory.Outcome, cm.Memory.Embedding); err != nil {
		return fmt.Errorf("writing memory: %w", err)
	}

	return tx.Commit(ctx)
}
