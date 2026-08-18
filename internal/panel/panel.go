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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/memory"
	"github.com/virajchogle/anchor/internal/store"
)

//go:embed index.html
var assets embed.FS

// Embedder is the subset of the embedding path the panel needs, so live recall
// can be driven from the browser without the panel depending on Bedrock.
type Embedder interface {
	Embed(ctx context.Context, text string) (store.Vector, error)
}

type Server struct {
	db    *pgxpool.Pool
	log   *slog.Logger
	embed Embedder
}

func New(db *pgxpool.Pool, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{db: db, log: log}
}

// WithEmbedder enables the live recall endpoint. Without one the panel still
// serves everything else, so a missing model never takes the page down.
func (s *Server) WithEmbedder(e Embedder) *Server { s.embed = e; return s }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/recall", s.handleRecall)
	mux.HandleFunc("GET /api/timetravel", s.handleTimeTravel)
	mux.HandleFunc("GET /api/incident", s.handleIncident)
	mux.HandleFunc("GET /api/live", s.handleLive)
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
	// Escalated counts intents the verifier could not settle. They stay PENDING
	// deliberately: the system refuses to guess and asks a human instead.
	Escalated int `json:"escalated"`
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
       (SELECT count(*) FROM playbooks),
       (SELECT count(*) FROM action_intents
         WHERE state='PENDING' AND outcome->>'disposition' = 'UNKNOWN')`).
		Scan(&t.Episodes, &t.Pinned, &t.Committed, &t.Pending, &t.Failed,
			&t.Playbooks, &t.Escalated)
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

// ---------------------------------------------------------------------------
// Live recall. This is the vector index made usable rather than described.
// ---------------------------------------------------------------------------

type RecallHit struct {
	EpisodeID  string  `json:"episode_id"`
	Symptom    string  `json:"symptom"`
	Narrative  string  `json:"narrative"`
	Outcome    string  `json:"outcome"`
	Status     string  `json:"status"`
	Distance   float64 `json:"distance"`
	Similarity float64 `json:"similarity"`
	Recency    float64 `json:"recency"`
	Salience   float64 `json:"salience"`
	Score      float64 `json:"score"`
	CreatedAt  string  `json:"created_at"`
}

type RecallResponse struct {
	Query     string      `json:"query"`
	Scope     string      `json:"scope"`
	TookMS    int64       `json:"took_ms"`
	Hits      []RecallHit `json:"hits"`
	Playbooks []Playbook  `json:"playbooks"`
	Note      string      `json:"note,omitempty"`
}

func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "prod"
	}
	if q == "" {
		writeJSON(w, RecallResponse{Hits: []RecallHit{}, Note: "enter a symptom to search memory"})
		return
	}
	if s.embed == nil {
		writeJSON(w, RecallResponse{Query: q, Hits: []RecallHit{},
			Note: "live recall needs an embedding model; set AWS credentials and redeploy"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	start := time.Now()
	st := memory.NewStore(s.db, s.embed)
	eps, err := st.RecallEpisodes(ctx, memory.Query{
		ScopeKey: scope, Text: q, K: 8,
		Statuses: []string{memory.StatusResolved, memory.StatusArchived, memory.StatusOpen},
	})
	if err != nil {
		s.fail(w, "recall", err)
		return
	}
	books, _ := st.RecallPlaybooks(ctx, scope, q, 3)

	resp := RecallResponse{Query: q, Scope: scope, TookMS: time.Since(start).Milliseconds(),
		Hits: []RecallHit{}, Playbooks: []Playbook{}}
	for _, e := range eps {
		resp.Hits = append(resp.Hits, RecallHit{
			EpisodeID: e.ID.String(), Symptom: e.Symptom, Narrative: e.Narrative,
			Outcome: e.Outcome, Status: e.Status, Distance: e.Distance,
			Similarity: e.Similarity, Recency: e.Recency, Salience: e.Salience,
			Score: e.Score, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	for _, b := range books {
		steps, _ := json.Marshal(b.Steps)
		resp.Playbooks = append(resp.Playbooks, Playbook{
			ID: b.ID.String(), Title: b.Title, Confidence: b.Confidence,
			DerivedFrom: len(b.DerivedFrom), Steps: string(steps),
		})
	}
	writeJSON(w, resp)
}

// ---------------------------------------------------------------------------
// Time travel. What the agent believed at an instant, read from MVCC.
// ---------------------------------------------------------------------------

type TimeTravelResponse struct {
	At       string    `json:"at"`
	Scope    string    `json:"scope"`
	Beliefs  []Episode `json:"beliefs"`
	Note     string    `json:"note,omitempty"`
	NowCount int       `json:"now_count"`
}

func (s *Server) handleTimeTravel(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "prod"
	}
	atRaw := r.URL.Query().Get("at")

	at := time.Now().Add(-5 * time.Minute)
	if atRaw != "" {
		parsed, err := time.Parse(time.RFC3339, atRaw)
		if err != nil {
			http.Error(w, "at must be RFC3339", http.StatusBadRequest)
			return
		}
		at = parsed
	}
	// A future timestamp is not a valid MVCC read, and the database error for it
	// is opaque, so reject it here with something an operator can act on.
	if at.After(time.Now()) {
		writeJSON(w, TimeTravelResponse{At: at.UTC().Format(time.RFC3339), Scope: scope,
			Beliefs: []Episode{}, Note: "cannot read the future; choose a past instant"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	st := memory.NewStore(s.db, s.embed)
	beliefs, err := st.AsOf(at).Beliefs(ctx, scope, 50)
	resp := TimeTravelResponse{At: at.UTC().Format(time.RFC3339), Scope: scope, Beliefs: []Episode{}}
	if err != nil {
		// Outside the garbage collection window is the common, explainable case.
		resp.Note = "no readable snapshot at that instant: " + err.Error()
		writeJSON(w, resp)
		return
	}
	for _, b := range beliefs {
		resp.Beliefs = append(resp.Beliefs, Episode{
			ID: b.ID.String(), ScopeKey: b.ScopeKey, Status: b.Status,
			Symptom: b.Symptom, Narrative: b.Narrative, Outcome: b.Outcome,
			Salience: b.Salience, CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	_ = s.db.QueryRow(ctx, `SELECT count(*) FROM episodes WHERE scope_key=$1`, scope).Scan(&resp.NowCount)
	writeJSON(w, resp)
}

// ---------------------------------------------------------------------------
// One incident, end to end.
// ---------------------------------------------------------------------------

type IncidentResponse struct {
	Episode Episode  `json:"episode"`
	Intents []Intent `json:"intents"`
	Note    string   `json:"note,omitempty"`
}

func (s *Server) handleIncident(w http.ResponseWriter, r *http.Request) {
	idRaw := r.URL.Query().Get("id")
	id, err := uuid.Parse(idRaw)
	if err != nil {
		http.Error(w, "id must be a UUID", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var e Episode
	err = s.db.QueryRow(ctx, `
SELECT episode_id::STRING, scope_key, status, symptom, narrative,
       coalesce(outcome,''), salience, expires_at IS NULL, created_at::STRING
  FROM episodes WHERE episode_id=$1`, id).
		Scan(&e.ID, &e.ScopeKey, &e.Status, &e.Symptom, &e.Narrative,
			&e.Outcome, &e.Salience, &e.Pinned, &e.CreatedAt)
	if err != nil {
		http.Error(w, "incident not found", http.StatusNotFound)
		return
	}

	rows, err := s.db.Query(ctx, `
SELECT idem_key, episode_id::STRING, action_type, state, coalesce(external_ref,''),
       attempts, args::STRING, coalesce(outcome::STRING,''), created_at::STRING,
       coalesce(resolved_at::STRING,'')
  FROM action_intents WHERE episode_id=$1 ORDER BY created_at`, id)
	if err != nil {
		s.fail(w, "incident intents", err)
		return
	}
	defer rows.Close()

	resp := IncidentResponse{Episode: e, Intents: []Intent{}}
	for rows.Next() {
		var i Intent
		if err := rows.Scan(&i.IdemKey, &i.EpisodeID, &i.ActionType, &i.State,
			&i.ExternalRef, &i.Attempts, &i.Args, &i.Outcome, &i.CreatedAt, &i.ResolvedAt); err != nil {
			s.fail(w, "incident intents", err)
			return
		}
		resp.Intents = append(resp.Intents, i)
	}
	writeJSON(w, resp)
}

// ---------------------------------------------------------------------------
// Live view: the protocol as it happens.
// ---------------------------------------------------------------------------

// Phase is one stage of the three-phase protocol as the UI renders it.
//
// State is one of: pending, active, done, alert, escalated. The distinction
// between alert and escalated matters: alert means the world may have changed
// and we have not recorded it yet, escalated means we looked and could not tell.
type Phase struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

type LiveResponse struct {
	EpisodeID  string  `json:"episode_id"`
	Symptom    string  `json:"symptom"`
	Status     string  `json:"status"`
	ActionType string  `json:"action_type"`
	IdemKey    string  `json:"idem_key"`
	Phases     []Phase `json:"phases"`
	Recalled   int     `json:"recalled"`
	StartedAt  string  `json:"started_at"`
	Note       string  `json:"note,omitempty"`
}

// handleLive describes the most recent incident as a phase pipeline.
//
// It is derived entirely from committed database state rather than from any
// in-process progress tracking, which means the UI stays correct even when the
// agent that started the work has died. That is the same property the protocol
// itself depends on.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		epID, symptom, status, createdAt string
	)
	err := s.db.QueryRow(ctx, `
SELECT episode_id::STRING, symptom, status, created_at::STRING
  FROM episodes ORDER BY created_at DESC LIMIT 1`).Scan(&epID, &symptom, &status, &createdAt)
	if err != nil {
		writeJSON(w, LiveResponse{Phases: []Phase{}, Note: "no incidents yet"})
		return
	}

	var (
		idemKey, actionType, state, extRef, outcome string
		attempts                                    int
	)
	hasIntent := true
	err = s.db.QueryRow(ctx, `
SELECT idem_key, action_type, state, coalesce(external_ref,''),
       coalesce(outcome::STRING,''), attempts
  FROM action_intents WHERE episode_id = $1 ORDER BY created_at DESC LIMIT 1`, epID).
		Scan(&idemKey, &actionType, &state, &extRef, &outcome, &attempts)
	if err != nil {
		hasIntent = false
	}

	// How many prior episodes existed when this one opened, which is what the
	// agent had available to recall.
	var recalled int
	_ = s.db.QueryRow(ctx,
		`SELECT count(*) FROM episodes WHERE created_at < (SELECT created_at FROM episodes WHERE episode_id=$1)`,
		epID).Scan(&recalled)

	unknown := strings.Contains(outcome, `"disposition":"UNKNOWN"`) ||
		strings.Contains(outcome, `"disposition": "UNKNOWN"`)

	resp := LiveResponse{
		EpisodeID: epID, Symptom: symptom, Status: status,
		ActionType: actionType, IdemKey: idemKey, Recalled: recalled,
		StartedAt: createdAt,
	}

	mk := func(key, label, st, detail string) Phase {
		return Phase{Key: key, Label: label, State: st, Detail: detail}
	}

	// 1. Opened
	resp.Phases = append(resp.Phases, mk("open", "Incident opened", "done",
		"Episode created with the symptom embedded. It must exist before the intent, "+
			"because the idempotency key hashes its id."))

	// 2. Recalled
	resp.Phases = append(resp.Phases, mk("recall", "Memory recalled", "done",
		fmt.Sprintf("%d prior episode(s) were available to search by cosine distance.", recalled)))

	if !hasIntent {
		resp.Phases = append(resp.Phases,
			mk("intend", "Intent written", "active", "waiting for the agent to decide"),
			mk("execute", "Executed", "pending", ""),
			mk("verify", "Verified", "pending", ""),
			mk("commit", "Committed atomically", "pending", ""))
		writeJSON(w, resp)
		return
	}

	// 3. Intended
	resp.Phases = append(resp.Phases, mk("intend", "Intent written", "done",
		fmt.Sprintf("Action %s recorded BEFORE touching the world. Key %s…",
			actionType, safeCut(idemKey, 24))))

	switch {
	case state == "COMMITTED":
		resp.Phases = append(resp.Phases,
			mk("execute", "Executed", "done", "The external call ran."),
			mk("verify", "Verified", "done", verifyDetail(outcome, extRef)),
			mk("commit", "Committed atomically", "done",
				"Intent resolution, world state and the memory row with its embedding, "+
					"in one serializable transaction."))
	case unknown:
		resp.Phases = append(resp.Phases,
			mk("execute", "Executed", "done", "The external call ran."),
			mk("verify", "Verified", "escalated",
				"Could not establish whether this took effect. Returning Unknown and "+
					"escalating rather than guessing. Attempt "+fmt.Sprint(attempts)+"."),
			mk("commit", "Committed atomically", "pending",
				"Deliberately not committed. A human decides."))
	case extRef == "":
		// The divergence window: acted, not recorded.
		resp.Phases = append(resp.Phases,
			mk("execute", "Executed", "alert",
				"The world may already have changed and nothing records it yet."),
			mk("verify", "Verified", "pending", "awaiting the reconciler"),
			mk("commit", "Committed atomically", "pending", ""))
	default:
		resp.Phases = append(resp.Phases,
			mk("execute", "Executed", "done", "The external call ran."),
			mk("verify", "Verified", "active", "checking external ground truth"),
			mk("commit", "Committed atomically", "pending", ""))
	}
	writeJSON(w, resp)
}

func verifyDetail(outcome, extRef string) string {
	src := "external evidence"
	switch {
	case strings.Contains(outcome, "audit_log"):
		src = "the CockroachDB Cloud audit log"
	case strings.Contains(outcome, "sql_user_list"):
		src = "a live resource listing"
	}
	if extRef != "" {
		return "Confirmed against " + src + ". Reference " + safeCut(extRef, 40) + "."
	}
	return "Confirmed against " + src + "."
}

func safeCut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
