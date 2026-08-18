// Package panel serves Anchor's observability page.
//
// Judges cannot score a memory layer they cannot see, so this exposes the whole
// path for each incident: what was recalled, what intent was written before
// acting, what the verifier concluded and on what evidence, and what finally
// committed. It also surfaces live contention from crdb_internal, because
// "what happens when two agents collide" is a question the design answers and
// therefore ought to be visible.
//
// Deliberately one page with no authentication, no framework, and no build step.
package panel

import (
	"context"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed index.html
var assets embed.FS

type Server struct {
	db  *pgxpool.Pool
	log *slog.Logger
}

func New(db *pgxpool.Pool, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{db: db, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	return s.withLogging(mux)
}

// withLogging emits structured JSON logs carrying request id, route, latency and
// row counts, and never memory content. Incident narratives can contain customer
// detail, so they stay out of logs by construction rather than by redaction.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"request_id", r.Header.Get("X-Request-Id"),
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "latency_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// State is everything the page renders.
type State struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Episodes    []Episode    `json:"episodes"`
	Intents     []Intent     `json:"intents"`
	Playbooks   []Playbook   `json:"playbooks"`
	Contention  []Contention `json:"contention"`
	Totals      Totals       `json:"totals"`
	Growth      []Bucket     `json:"growth"`
	Evidence    []Evidence   `json:"evidence"`
}

// Bucket is one time slice of memory accumulation.
type Bucket struct {
	At         string `json:"at"`
	Count      int    `json:"count"`
	Cumulative int    `json:"cumulative"`
}

// Evidence counts committed intents by the source that settled them. This is the
// distribution that matters most: an intent settled by nothing is an intent that
// should never have been marked done.
type Evidence struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

type Totals struct {
	Episodes  int `json:"episodes"`
	Pinned    int `json:"pinned"`
	Committed int `json:"committed"`
	Pending   int `json:"pending"`
	Failed    int `json:"failed"`
	Playbooks int `json:"playbooks"`
}

type Episode struct {
	ID        string  `json:"episode_id"`
	ScopeKey  string  `json:"scope_key"`
	Status    string  `json:"status"`
	Symptom   string  `json:"symptom"`
	Narrative string  `json:"narrative"`
	Outcome   string  `json:"outcome"`
	Salience  float64 `json:"salience"`
	Pinned    bool    `json:"pinned"`
	CreatedAt string  `json:"created_at"`
}

type Intent struct {
	IdemKey     string `json:"idem_key"`
	EpisodeID   string `json:"episode_id"`
	ActionType  string `json:"action_type"`
	State       string `json:"state"`
	ExternalRef string `json:"external_ref"`
	Attempts    int    `json:"attempts"`
	Args        string `json:"args"`
	Outcome     string `json:"outcome"`
	CreatedAt   string `json:"created_at"`
	ResolvedAt  string `json:"resolved_at"`
}

type Playbook struct {
	ID          string  `json:"playbook_id"`
	Title       string  `json:"title"`
	Confidence  float64 `json:"confidence"`
	DerivedFrom int     `json:"derived_from_count"`
	Steps       string  `json:"steps"`
}

type Contention struct {
	Table       string `json:"table"`
	Index       string `json:"index"`
	Count       int64  `json:"count"`
	WaitSeconds string `json:"cumulative_wait"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	st := State{GeneratedAt: time.Now().UTC()}
	var err error
	if st.Episodes, err = s.episodes(ctx); err != nil {
		s.fail(w, "episodes", err)
		return
	}
	if st.Intents, err = s.intents(ctx); err != nil {
		s.fail(w, "intents", err)
		return
	}
	if st.Playbooks, err = s.playbooks(ctx); err != nil {
		s.fail(w, "playbooks", err)
		return
	}
	// Contention is best-effort: crdb_internal access can be restricted, and a
	// missing diagnostic must not take down the page.
	if st.Contention, err = s.contention(ctx); err != nil || st.Contention == nil {
		st.Contention = []Contention{}
	}
	st.Totals, _ = s.totals(ctx)

	if st.Growth, err = s.growth(ctx); err != nil || st.Growth == nil {
		st.Growth = []Bucket{}
	}
	if st.Evidence, err = s.evidence(ctx); err != nil || st.Evidence == nil {
		st.Evidence = []Evidence{}
	}

	writeJSON(w, st)
}

func (s *Server) episodes(ctx context.Context) ([]Episode, error) {
	rows, err := s.db.Query(ctx, `
SELECT episode_id::STRING, scope_key, status, symptom, narrative,
       coalesce(outcome,''), salience, expires_at IS NULL,
       created_at::STRING
  FROM episodes ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Episode{}
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.ID, &e.ScopeKey, &e.Status, &e.Symptom, &e.Narrative,
			&e.Outcome, &e.Salience, &e.Pinned, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Server) intents(ctx context.Context) ([]Intent, error) {
	rows, err := s.db.Query(ctx, `
SELECT idem_key, episode_id::STRING, action_type, state,
       coalesce(external_ref,''), attempts, args::STRING,
       coalesce(outcome::STRING,''), created_at::STRING,
       coalesce(resolved_at::STRING,'')
  FROM action_intents ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Intent{}
	for rows.Next() {
		var i Intent
		if err := rows.Scan(&i.IdemKey, &i.EpisodeID, &i.ActionType, &i.State,
			&i.ExternalRef, &i.Attempts, &i.Args, &i.Outcome, &i.CreatedAt, &i.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Server) playbooks(ctx context.Context) ([]Playbook, error) {
	rows, err := s.db.Query(ctx, `
SELECT playbook_id::STRING, title, confidence,
       array_length(derived_from, 1), steps::STRING
  FROM playbooks ORDER BY updated_at DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Playbook{}
	for rows.Next() {
		var p Playbook
		if err := rows.Scan(&p.ID, &p.Title, &p.Confidence, &p.DerivedFrom, &p.Steps); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// contention reads CockroachDB's own contention view.
//
// As of v26.2 crdb_internal is restricted:
//
//	ERROR: Access to crdb_internal and system is restricted (SQLSTATE 42501)
//	HINT: set the session variable allow_unsafe_internals = true (not recommended)
//
// Anchor does not enable that by default. A panel is not a good enough reason to
// turn on a setting the database itself labels unsafe, and doing it silently
// would be worse. Set ANCHOR_UNSAFE_INTERNALS=1 to opt in; otherwise the panel
// reports the view as restricted rather than pretending there is no contention.
func (s *Server) contention(ctx context.Context) ([]Contention, error) {
	if os.Getenv("ANCHOR_UNSAFE_INTERNALS") != "1" {
		return []Contention{{
			Table:       "restricted",
			Index:       "crdb_internal",
			WaitSeconds: "set ANCHOR_UNSAFE_INTERNALS=1 to enable",
		}}, nil
	}

	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	// Scoped to this connection only, never a cluster-wide setting change.
	if _, err := conn.Exec(ctx, "SET allow_unsafe_internals = true"); err != nil {
		return nil, err
	}

	rows, err := conn.Query(ctx, `
SELECT coalesce(t.name, ce.table_id::STRING),
       ce.index_id::STRING,
       sum(ce.num_contention_events)::INT8,
       sum(ce.cumulative_contention_time)::STRING
  FROM crdb_internal.cluster_contention_events ce
  LEFT JOIN crdb_internal.tables t ON t.table_id = ce.table_id
 GROUP BY 1, 2
 ORDER BY 3 DESC
 LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Contention{}
	for rows.Next() {
		var c Contention
		if err := rows.Scan(&c.Table, &c.Index, &c.Count, &c.WaitSeconds); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Server) totals(ctx context.Context) (Totals, error) {
	var t Totals
	err := s.db.QueryRow(ctx, `
SELECT (SELECT count(*) FROM episodes),
       (SELECT count(*) FROM episodes WHERE expires_at IS NULL),
       (SELECT count(*) FROM action_intents WHERE state='COMMITTED'),
       (SELECT count(*) FROM action_intents WHERE state='PENDING'),
       (SELECT count(*) FROM action_intents WHERE state='FAILED'),
       (SELECT count(*) FROM playbooks)`).
		Scan(&t.Episodes, &t.Pinned, &t.Committed, &t.Pending, &t.Failed, &t.Playbooks)
	return t, err
}

// growth returns one point per episode, cumulative.
//
// Bucketing by time was the first attempt and it was wrong for this data: a live
// demo creates several episodes inside one minute, so every bucketing granularity
// collapsed to a single point and the chart had nothing to draw. One point per
// episode always renders, and the x-axis still carries real timestamps.
func (s *Server) growth(ctx context.Context) ([]Bucket, error) {
	rows, err := s.db.Query(ctx, `
SELECT created_at::STRING
  FROM episodes
 ORDER BY created_at DESC
 LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stamps []string
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		stamps = append(stamps, at)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Newest first from the query so LIMIT keeps recent history; reverse to plot
	// forward in time, then accumulate.
	for i, j := 0, len(stamps)-1; i < j; i, j = i+1, j-1 {
		stamps[i], stamps[j] = stamps[j], stamps[i]
	}
	out := make([]Bucket, 0, len(stamps))
	for i, at := range stamps {
		out = append(out, Bucket{At: at, Count: 1, Cumulative: i + 1})
	}
	return out, nil
}

func (s *Server) evidence(ctx context.Context) ([]Evidence, error) {
	rows, err := s.db.Query(ctx, `
SELECT coalesce(outcome->>'evidence', 'unattributed'), count(*)::INT
  FROM action_intents
 WHERE state = 'COMMITTED'
 GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Evidence
	for rows.Next() {
		var e Evidence
		if err := rows.Scan(&e.Source, &e.Count); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("panel query failed", "query", what, "error", err)
	http.Error(w, "query failed: "+what, http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
