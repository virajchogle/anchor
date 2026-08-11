package memory

import (
	"context"
	"fmt"
	"time"
)

// DecayOptions tunes memory decay.
type DecayOptions struct {
	// HalfLife is how long it takes an untouched episode's salience to halve.
	HalfLife time.Duration

	// Floor is the salience below which an episode stops being worth recalling
	// and becomes eligible for expiry.
	Floor float64

	// Grace is how long an episode sits below the floor before TTL may reap it,
	// giving an operator a window to notice and rescue something.
	Grace time.Duration
}

func (o *DecayOptions) applyDefaults() {
	if o.HalfLife <= 0 {
		o.HalfLife = 30 * 24 * time.Hour
	}
	if o.Floor <= 0 {
		o.Floor = 0.1
	}
	if o.Grace <= 0 {
		o.Grace = 7 * 24 * time.Hour
	}
}

// DecayStats reports what a decay pass changed.
type DecayStats struct {
	Decayed         int
	MarkedForExpiry int
	PinnedUntouched int
}

// Decay reduces salience with age and schedules faded memories for expiry.
//
// The critical guard is `expires_at IS NOT NULL`. A NULL expires_at means the
// episode is pinned because an action committed against it, and writing a
// timestamp there would make the row TTL-eligible. That is exactly the failure
// the schema was designed to prevent: the row-level TTL job would then try to
// delete an episode that action_intents references and fail with 23503 on every
// run. Decay must never un-pin a memory.
//
// Salience itself decays on pinned rows too. A pinned episode can become less
// worth recalling; it just never disappears.
func (s *Store) Decay(ctx context.Context, scopeKey string, opts DecayOptions) (DecayStats, error) {
	opts.applyDefaults()
	var st DecayStats

	// Exponential decay expressed in SQL so the whole scope updates in one
	// statement rather than a read-modify-write loop that would contend with
	// live agents.
	halfLives := fmt.Sprintf("(extract(epoch FROM now() - created_at) / %f)", opts.HalfLife.Seconds())
	tag, err := s.db.Exec(ctx, `
UPDATE episodes
   SET salience = greatest(0.0, least(1.0, salience * pow(0.5, `+halfLives+`)))
 WHERE scope_key = $1 AND status <> $2`, scopeKey, StatusOpen)
	if err != nil {
		return st, fmt.Errorf("memory: decaying salience: %w", err)
	}
	st.Decayed = int(tag.RowsAffected())

	// Schedule faded, unpinned memories for TTL.
	tag, err = s.db.Exec(ctx, `
UPDATE episodes
   SET expires_at = now() + $3::INTERVAL
 WHERE scope_key = $1
   AND salience < $2
   AND expires_at IS NOT NULL`,
		scopeKey, opts.Floor, fmt.Sprintf("%d seconds", int(opts.Grace.Seconds())))
	if err != nil {
		return st, fmt.Errorf("memory: scheduling expiry: %w", err)
	}
	st.MarkedForExpiry = int(tag.RowsAffected())

	// Report how many memories are immune, so the panel can show that the audit
	// trail is growing rather than silently eroding.
	if err := s.db.QueryRow(ctx, `
SELECT count(*) FROM episodes WHERE scope_key = $1 AND expires_at IS NULL`,
		scopeKey).Scan(&st.PinnedUntouched); err != nil {
		return st, err
	}

	return st, nil
}

// Reinforce raises an episode's salience when it proves useful, which is what
// makes recall improve rather than merely age. Called when a recalled episode
// contributes to a resolution.
func (s *Store) Reinforce(ctx context.Context, episodeID fmt.Stringer, boost float64) error {
	if boost <= 0 {
		boost = 0.2
	}
	_, err := s.db.Exec(ctx, `
UPDATE episodes
   SET salience = least(1.0, salience + $2)
 WHERE episode_id = $1`, episodeID.String(), boost)
	if err != nil {
		return fmt.Errorf("memory: reinforcing episode: %w", err)
	}
	return nil
}
