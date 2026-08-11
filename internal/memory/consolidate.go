package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/virajchogle/anchor/internal/store"
)

// ConsolidateOptions tunes promotion of episodes into playbooks.
type ConsolidateOptions struct {
	// MinEpisodes is how many similar incidents must exist before they justify a
	// playbook. Two is coincidence; the default of three is the point at which
	// "this keeps happening" becomes a defensible claim.
	MinEpisodes int

	// MaxDistance is the cosine distance within which two episodes count as the
	// same class of incident.
	MaxDistance float64

	// Archive moves consolidated episodes out of recall. They are never deleted,
	// because the playbook's provenance points at them.
	Archive bool
}

func (o *ConsolidateOptions) applyDefaults() {
	if o.MinEpisodes <= 0 {
		o.MinEpisodes = 3
	}
	if o.MaxDistance <= 0 {
		o.MaxDistance = 0.25
	}
}

type candidate struct {
	id        uuid.UUID
	symptom   string
	narrative string
	outcome   string
	embedding store.Vector
	createdAt time.Time
}

// Consolidate promotes recurring episodes into playbooks.
//
// Semantic memory here is derived, not generated. A playbook's steps come from
// the action_intents that actually committed against its source episodes, and
// derived_from records exactly which episodes produced it. Every step is
// traceable to something the agent really did, which is the difference between
// institutional memory and a plausible-sounding summary.
func (s *Store) Consolidate(ctx context.Context, scopeKey string, opts ConsolidateOptions) ([]Playbook, error) {
	opts.applyDefaults()

	cands, err := s.consolidationCandidates(ctx, scopeKey)
	if err != nil {
		return nil, err
	}
	if len(cands) < opts.MinEpisodes {
		return nil, nil
	}

	groups := groupBySimilarity(cands, opts)

	var created []Playbook
	for _, g := range groups {
		if len(g) < opts.MinEpisodes {
			continue
		}
		pb, err := s.writePlaybook(ctx, scopeKey, g, opts)
		if err != nil {
			return created, err
		}
		created = append(created, *pb)
	}
	return created, nil
}

// consolidationCandidates loads resolved episodes that have not yet been
// consolidated. Archived episodes are excluded because they already belong to a
// playbook.
func (s *Store) consolidationCandidates(ctx context.Context, scopeKey string) ([]candidate, error) {
	rows, err := s.db.Query(ctx, `
SELECT episode_id, symptom, narrative, coalesce(outcome, ''), embedding, created_at
  FROM episodes
 WHERE scope_key = $1 AND status = $2
 ORDER BY created_at`, scopeKey, StatusResolved)
	if err != nil {
		return nil, fmt.Errorf("memory: loading consolidation candidates: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.symptom, &c.narrative, &c.outcome,
			&c.embedding, &c.createdAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// groupBySimilarity performs single-pass greedy clustering by cosine distance.
//
// Greedy rather than k-means because the number of incident classes is not known
// ahead of time and, more practically, because a clustering that an operator
// cannot explain is worse than a slightly suboptimal one they can.
func groupBySimilarity(cands []candidate, opts ConsolidateOptions) [][]candidate {
	used := make([]bool, len(cands))
	var groups [][]candidate

	for i := range cands {
		if used[i] {
			continue
		}
		group := []candidate{cands[i]}
		used[i] = true

		for j := i + 1; j < len(cands); j++ {
			if used[j] {
				continue
			}
			if cosineDistance(cands[i].embedding, cands[j].embedding) <= opts.MaxDistance {
				group = append(group, cands[j])
				used[j] = true
			}
		}
		groups = append(groups, group)
	}
	return groups
}

func cosineDistance(a, b store.Vector) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.Inf(1)
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return math.Inf(1)
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// writePlaybook creates the playbook and archives its sources in one
// transaction, so a playbook can never exist pointing at episodes that were
// never archived, or vice versa.
func (s *Store) writePlaybook(ctx context.Context, scopeKey string, group []candidate, opts ConsolidateOptions) (*Playbook, error) {
	ids := make([]uuid.UUID, len(group))
	for i, c := range group {
		ids[i] = c.id
	}

	steps, err := s.stepsFromIntents(ctx, ids)
	if err != nil {
		return nil, err
	}

	// The playbook embedding is the normalized centroid of its member episodes.
	// Using the centroid rather than an embedding of the title means recall
	// compares a live symptom against the actual shape of past symptoms, and it
	// costs no additional model call.
	centroid := normalizedCentroid(group)

	title := fmt.Sprintf("Recurring: %s", group[0].symptom)
	confidence := confidenceFor(group, opts)

	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	pb := Playbook{
		ScopeKey: scopeKey, Status: StatusActivePlaybook, Title: title,
		Steps: steps, DerivedFrom: ids, Confidence: confidence,
	}
	err = tx.QueryRow(ctx, `
INSERT INTO playbooks (scope_key, status, title, steps, derived_from, confidence, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING playbook_id, updated_at`,
		scopeKey, StatusActivePlaybook, title, stepsJSON, ids, confidence, centroid).
		Scan(&pb.ID, &pb.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("memory: inserting playbook: %w", err)
	}

	if opts.Archive {
		// Archived, never deleted: the playbook's derived_from points here, and
		// the time-travel path needs these rows to answer what the agent knew.
		if _, err := tx.Exec(ctx,
			`UPDATE episodes SET status = $1 WHERE episode_id = ANY($2)`,
			StatusArchived, ids); err != nil {
			return nil, fmt.Errorf("memory: archiving consolidated episodes: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &pb, nil
}

// stepsFromIntents derives playbook steps from actions that actually committed.
//
// Only COMMITTED intents are considered. A playbook that recommended an action
// which was never verified to have worked would be worse than no playbook.
func (s *Store) stepsFromIntents(ctx context.Context, episodeIDs []uuid.UUID) ([]Step, error) {
	rows, err := s.db.Query(ctx, `
SELECT action_type, args, count(*)
  FROM action_intents
 WHERE episode_id = ANY($1) AND state = 'COMMITTED'
 GROUP BY action_type, args
 ORDER BY count(*) DESC, action_type`, episodeIDs)
	if err != nil {
		return nil, fmt.Errorf("memory: deriving steps from intents: %w", err)
	}
	defer rows.Close()

	var steps []Step
	for rows.Next() {
		var st Step
		var raw []byte
		if err := rows.Scan(&st.ActionType, &raw, &st.Observed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &st.Args); err != nil {
			return nil, fmt.Errorf("memory: decoding intent args: %w", err)
		}
		steps = append(steps, st)
	}
	return steps, rows.Err()
}

func normalizedCentroid(group []candidate) store.Vector {
	if len(group) == 0 {
		return nil
	}
	out := make(store.Vector, len(group[0].embedding))
	for _, c := range group {
		for i := range c.embedding {
			out[i] += c.embedding[i]
		}
	}
	var norm float64
	for _, f := range out {
		norm += float64(f) * float64(f)
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range out {
			out[i] = float32(float64(out[i]) / norm)
		}
	}
	return out
}

// confidenceFor scores a playbook on how much evidence backs it.
//
// Two factors: how many incidents it was derived from, saturating at twice the
// minimum, and how tightly clustered they were. A large loose group and a small
// tight group are both less trustworthy than a large tight one.
func confidenceFor(group []candidate, opts ConsolidateOptions) float64 {
	support := float64(len(group)) / float64(opts.MinEpisodes*2)
	if support > 1 {
		support = 1
	}

	var total float64
	var pairs int
	for i := range group {
		for j := i + 1; j < len(group); j++ {
			total += cosineDistance(group[i].embedding, group[j].embedding)
			pairs++
		}
	}
	tightness := 1.0
	if pairs > 0 {
		tightness = 1 - (total/float64(pairs))/opts.MaxDistance
		if tightness < 0 {
			tightness = 0
		}
	}

	c := 0.5*support + 0.5*tightness
	return math.Round(c*1000) / 1000
}

func decodeSteps(raw []byte, out *[]Step) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("memory: decoding playbook steps: %w", err)
	}
	return nil
}
