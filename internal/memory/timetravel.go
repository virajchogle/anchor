package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Snapshot reads memory as it existed at a past instant, using CockroachDB's
// MVCC history rather than an application-maintained audit table.
//
// This answers the question that matters after an agent does something
// surprising: not "what is true now" but "what did the agent believe when it
// decided". An audit table can only answer that if someone remembered to write
// to it; AS OF SYSTEM TIME answers it for every row that ever existed, including
// ones nobody thought to log.
type Snapshot struct {
	store *Store
	at    time.Time
	// clause is the pre-rendered AS OF SYSTEM TIME fragment.
	clause string
}

// AsOf returns a read-only view of memory at time t.
//
// The timestamp is formatted into the SQL string rather than bound as a
// parameter. That is not a shortcut: CockroachDB rejects placeholders in AS OF
// SYSTEM TIME entirely, including inside with_min_timestamp(), verified on
// v26.2.5:
//
//	ERROR: AS OF SYSTEM TIME: only constant expressions, with_min_timestamp,
//	with_max_staleness, or follower_read_timestamp are allowed (SQLSTATE XXUUU)
//
// Injection is not possible here because the input is a time.Time, not a
// string. It is rendered through a fixed layout, so no caller-controlled text
// can reach the query.
func (s *Store) AsOf(t time.Time) *Snapshot {
	return &Snapshot{
		store:  s,
		at:     t,
		clause: fmt.Sprintf("AS OF SYSTEM TIME '%s'", t.UTC().Format("2006-01-02 15:04:05.000000-07:00")),
	}
}

// At reports the instant this snapshot reads from.
func (sn *Snapshot) At() time.Time { return sn.at }

// Beliefs returns the episodes visible in this scope at the snapshot instant.
//
// Ordered by salience rather than by vector distance, because the question is
// "what did the agent have available to recall", not "what matches this text".
func (sn *Snapshot) Beliefs(ctx context.Context, scopeKey string, limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := fmt.Sprintf(`
SELECT episode_id, scope_key, status, symptom, narrative,
       coalesce(outcome, ''), salience, created_at
  FROM episodes %s
 WHERE scope_key = $1
 ORDER BY salience DESC, created_at DESC
 LIMIT $2`, sn.clause)

	rows, err := sn.store.db.Query(ctx, sql, scopeKey, limit)
	if err != nil {
		return nil, fmt.Errorf("memory: reading beliefs as of %s: %w", sn.at, err)
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.ID, &e.ScopeKey, &e.Status, &e.Symptom,
			&e.Narrative, &e.Outcome, &e.Salience, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// IntentState reports what an intent looked like at the snapshot instant, which
// is how an operator reconstructs whether the agent had already committed an
// action when it made a later decision.
func (sn *Snapshot) IntentState(ctx context.Context, idemKey string) (state, externalRef string, err error) {
	sql := fmt.Sprintf(`
SELECT state, coalesce(external_ref, '')
  FROM action_intents %s
 WHERE idem_key = $1`, sn.clause)

	err = sn.store.db.QueryRow(ctx, sql, idemKey).Scan(&state, &externalRef)
	if err != nil {
		return "", "", fmt.Errorf("memory: reading intent %s as of %s: %w", idemKey, sn.at, err)
	}
	return state, externalRef, nil
}

// EpisodeAt reads a single episode as it stood at the snapshot instant.
func (sn *Snapshot) EpisodeAt(ctx context.Context, id uuid.UUID) (Episode, error) {
	sql := fmt.Sprintf(`
SELECT episode_id, scope_key, status, symptom, narrative,
       coalesce(outcome, ''), salience, created_at
  FROM episodes %s
 WHERE episode_id = $1`, sn.clause)

	var e Episode
	err := sn.store.db.QueryRow(ctx, sql, id).Scan(&e.ID, &e.ScopeKey, &e.Status,
		&e.Symptom, &e.Narrative, &e.Outcome, &e.Salience, &e.CreatedAt)
	if err != nil {
		return Episode{}, fmt.Errorf("memory: reading episode %s as of %s: %w", id, sn.at, err)
	}
	return e, nil
}
