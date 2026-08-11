// Package memory implements Anchor's memory lifecycle: recall, consolidation,
// decay, and time travel.
//
// The retrieval design is shaped by one hard constraint of CockroachDB's vector
// index: only prefix columns may be constrained in WHERE. Adding a predicate on
// salience or created_at silently disqualifies the index and degrades to a full
// scan with no warning. So the SQL filters on prefix columns only, over-fetches,
// and applies salience and recency in application code afterwards.
package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/store"
)

// Embedder turns text into an embedding. Declared here, where it is consumed.
type Embedder interface {
	Embed(ctx context.Context, text string) (store.Vector, error)
}

// Episode statuses. These are prefix column values, so recall filters on them
// through the vector index rather than after it.
const (
	StatusOpen     = "open"
	StatusResolved = "resolved"
	StatusArchived = "archived"
)

type Store struct {
	db    *pgxpool.Pool
	embed Embedder
}

func NewStore(db *pgxpool.Pool, embed Embedder) *Store {
	return &Store{db: db, embed: embed}
}

// Episode is a recalled memory with its ranking components exposed. The
// components are kept separate rather than collapsed into one number so the
// observability panel can show why something was recalled.
type Episode struct {
	ID        uuid.UUID
	ScopeKey  string
	Status    string
	Symptom   string
	Narrative string
	Outcome   string
	Salience  float64
	CreatedAt time.Time

	Distance   float64 // cosine distance from the query, straight from the index
	Similarity float64 // 1 - Distance
	Recency    float64 // exponential decay on age
	Score      float64 // weighted combination actually used for ranking
}

// Query describes a recall request.
type Query struct {
	ScopeKey string
	// Statuses are prefix column values. Empty means resolved and archived,
	// which is what "what has happened like this before" should search: open
	// episodes are the incident currently in flight, not prior experience.
	Statuses []string
	Text     string
	K        int

	// MinSalience and MaxAge are applied AFTER retrieval, in Go. Putting them in
	// the WHERE clause would disqualify the vector index.
	MinSalience float64
	MaxAge      time.Duration

	// HalfLife controls how fast recency weight falls off. Zero means 30 days.
	HalfLife time.Duration

	// OverFetch multiplies K when querying the index, so post-retrieval
	// filtering still has candidates left. Zero means 4.
	OverFetch int
}

// Ranking weights. Similarity dominates because it is the only signal that
// actually measures relevance; recency and salience break ties among things
// that are already similar.
const (
	weightSimilarity = 0.60
	weightRecency    = 0.20
	weightSalience   = 0.20
)

func (q *Query) applyDefaults() {
	if len(q.Statuses) == 0 {
		q.Statuses = []string{StatusResolved, StatusArchived}
	}
	if q.K <= 0 {
		q.K = 5
	}
	if q.HalfLife <= 0 {
		q.HalfLife = 30 * 24 * time.Hour
	}
	if q.OverFetch <= 0 {
		q.OverFetch = 4
	}
}

// RecallEpisodes finds prior incidents similar to the query text.
func (s *Store) RecallEpisodes(ctx context.Context, q Query) ([]Episode, error) {
	q.applyDefaults()

	vec, err := s.embed.Embed(ctx, q.Text)
	if err != nil {
		return nil, fmt.Errorf("memory: embedding recall query: %w", err)
	}
	if err := vec.Validate(); err != nil {
		return nil, err
	}

	sql, args := recallEpisodesSQL(q, vec)
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: recalling episodes: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	var out []Episode
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.ID, &e.ScopeKey, &e.Status, &e.Symptom,
			&e.Narrative, &e.Outcome, &e.Salience, &e.CreatedAt, &e.Distance); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return rankAndTrim(out, q, now), nil
}

// recallEpisodesSQL builds the retrieval query.
//
// The status list is expanded into explicit placeholders rather than passed as
// an array with = ANY($n). Verified on v26.2.5: an IN list produces one prefix
// span per value and the vector index is used. The placeholders are generated
// from the slice length, never from its contents, so this is not string
// interpolation of user data.
func recallEpisodesSQL(q Query, vec store.Vector) (string, []any) {
	args := []any{vec, q.ScopeKey}
	placeholders := make([]string, len(q.Statuses))
	for i, st := range q.Statuses {
		args = append(args, st)
		placeholders[i] = fmt.Sprintf("$%d", len(args))
	}
	args = append(args, q.K*q.OverFetch)

	sql := fmt.Sprintf(`
SELECT episode_id, scope_key, status, symptom, narrative,
       coalesce(outcome, ''), salience, created_at,
       embedding <=> $1 AS distance
  FROM episodes
 WHERE scope_key = $2 AND status IN (%s)
 ORDER BY embedding <=> $1
 LIMIT $%d`, strings.Join(placeholders, ", "), len(args))

	return sql, args
}

// rankAndTrim applies the filters the vector index could not express.
func rankAndTrim(in []Episode, q Query, now time.Time) []Episode {
	out := in[:0]
	for _, e := range in {
		if e.Salience < q.MinSalience {
			continue
		}
		age := now.Sub(e.CreatedAt)
		if q.MaxAge > 0 && age > q.MaxAge {
			continue
		}

		e.Similarity = 1 - e.Distance
		e.Recency = math.Exp(-math.Ln2 * age.Hours() / q.HalfLife.Hours())
		e.Score = weightSimilarity*e.Similarity +
			weightRecency*e.Recency +
			weightSalience*e.Salience
		out = append(out, e)
	}

	// The index returned rows in distance order; re-sort by combined score.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	if len(out) > q.K {
		out = out[:q.K]
	}
	return out
}

// Playbook is consolidated semantic memory: a procedure derived from episodes.
type Playbook struct {
	ID          uuid.UUID
	ScopeKey    string
	Status      string
	Title       string
	Steps       []Step
	DerivedFrom []uuid.UUID
	Confidence  float64
	UpdatedAt   time.Time

	Distance   float64
	Similarity float64
}

// Step is one action in a playbook, traceable to the intents that produced it.
type Step struct {
	ActionType string         `json:"action_type"`
	Args       map[string]any `json:"args"`
	// Observed is how many of the source episodes took this step. It is the
	// evidence behind the step, not a guess by a model.
	Observed int `json:"observed"`
}

// RecallPlaybooks searches consolidated procedures for this scope.
func (s *Store) RecallPlaybooks(ctx context.Context, scopeKey, text string, k int) ([]Playbook, error) {
	if k <= 0 {
		k = 3
	}
	vec, err := s.embed.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("memory: embedding playbook query: %w", err)
	}

	rows, err := s.db.Query(ctx, `
SELECT playbook_id, scope_key, status, title, steps, derived_from, confidence, updated_at,
       embedding <=> $1 AS distance
  FROM playbooks
 WHERE scope_key = $2 AND status = $3
 ORDER BY embedding <=> $1
 LIMIT $4`, vec, scopeKey, StatusActivePlaybook, k)
	if err != nil {
		return nil, fmt.Errorf("memory: recalling playbooks: %w", err)
	}
	defer rows.Close()

	var out []Playbook
	for rows.Next() {
		var p Playbook
		var steps []byte
		if err := rows.Scan(&p.ID, &p.ScopeKey, &p.Status, &p.Title, &steps,
			&p.DerivedFrom, &p.Confidence, &p.UpdatedAt, &p.Distance); err != nil {
			return nil, err
		}
		if err := decodeSteps(steps, &p.Steps); err != nil {
			return nil, err
		}
		p.Similarity = 1 - p.Distance
		out = append(out, p)
	}
	return out, rows.Err()
}

// StatusActivePlaybook is the prefix value for playbooks in use.
const StatusActivePlaybook = "active"
