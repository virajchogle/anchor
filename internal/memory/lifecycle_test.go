package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/memory"
	"github.com/virajchogle/anchor/internal/testsupport"
)

// insertResolved creates a resolved episode plus a committed intent against it,
// mirroring what phase 3 leaves behind: the episode is pinned (expires_at NULL)
// because an action committed against it.
//
// Note on embeddings: HashEmbedder is deterministic on text but not semantic, so
// identical symptom text produces identical vectors (distance 0) and different
// text produces near-orthogonal ones. That is exactly what consolidation
// clustering needs to be tested deterministically.
func insertResolved(t *testing.T, pool *pgxpool.Pool, agentID uuid.UUID, symptom string, nodes int) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	v, err := fakeworld.HashEmbedder{}.Embed(ctx, symptom)
	if err != nil {
		t.Fatal(err)
	}

	var epID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, outcome, salience, embedding, expires_at)
VALUES ('prod', $1, $2, 'scaled the cluster', 'resolved', 0.7, $3, NULL)
RETURNING episode_id`, memory.StatusResolved, symptom, v).Scan(&epID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO action_intents (idem_key, episode_id, agent_id, action_type, args, state, outcome, resolved_at)
VALUES ($1, $2, $3, 'scale_cluster', $4, 'COMMITTED', '{"ok":true}', now())`,
		"key-"+uuid.NewString(), epID, agentID,
		[]byte(`{"cluster_id":"c-1","nodes":`+itoa(nodes)+`}`)); err != nil {
		t.Fatal(err)
	}
	return epID
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newAgent(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (agent_id, name, scope) VALUES ($1, 'anchor', ARRAY['prod'])`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestConsolidate_DerivesPlaybookWithProvenance is the "third incident resolves
// faster" mechanism. Three incidents of one class become a playbook whose steps
// come from intents that actually committed.
func TestConsolidate_DerivesPlaybookWithProvenance(t *testing.T) {
	ctx := context.Background()
	pool, _ := testsupport.NewDB(t)
	agentID := newAgent(t, pool)
	st := memory.NewStore(pool, fakeworld.HashEmbedder{})

	const symptom = "p99 latency above SLO on orders cluster"
	var sources []uuid.UUID
	for i := 0; i < 3; i++ {
		sources = append(sources, insertResolved(t, pool, agentID, symptom, 5))
	}
	// An unrelated incident that must not be swept into the same playbook.
	other := insertResolved(t, pool, agentID, "disk usage climbing on billing cluster", 3)

	books, err := st.Consolidate(ctx, "prod", memory.ConsolidateOptions{
		MinEpisodes: 3, MaxDistance: 0.25, Archive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 {
		t.Fatalf("expected exactly 1 playbook, got %d", len(books))
	}

	pb := books[0]
	if len(pb.DerivedFrom) != 3 {
		t.Errorf("provenance should name all 3 source episodes, got %d", len(pb.DerivedFrom))
	}
	for _, id := range pb.DerivedFrom {
		if id == other {
			t.Error("unrelated episode was consolidated into the playbook")
		}
	}
	if len(pb.Steps) == 0 {
		t.Fatal("playbook has no steps; they should be derived from committed intents")
	}
	if pb.Steps[0].ActionType != "scale_cluster" {
		t.Errorf("step action type = %q, want scale_cluster", pb.Steps[0].ActionType)
	}
	if pb.Steps[0].Observed != 3 {
		t.Errorf("step should record it was observed 3 times, got %d", pb.Steps[0].Observed)
	}
	if pb.Confidence <= 0 || pb.Confidence > 1 {
		t.Errorf("confidence out of range: %v", pb.Confidence)
	}

	// Sources archived, not deleted: the provenance must stay resolvable.
	for _, id := range sources {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM episodes WHERE episode_id=$1`, id).Scan(&status); err != nil {
			t.Fatalf("source episode %s was deleted, breaking provenance: %v", id, err)
		}
		if status != memory.StatusArchived {
			t.Errorf("source episode status = %q, want archived", status)
		}
	}

	// The playbook is now recallable.
	found, err := st.RecallPlaybooks(ctx, "prod", symptom, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("consolidated playbook was not recallable")
	}
	if found[0].ID != pb.ID {
		t.Errorf("recalled the wrong playbook: got %s want %s", found[0].ID, pb.ID)
	}
}

// TestDecay_NeverUnpinsCommittedEpisodes guards the single most dangerous
// interaction in the schema. An episode with a committed action has expires_at
// NULL. If decay ever wrote a timestamp there, the row-level TTL job would try
// to delete a row that action_intents references and fail with 23503 on every
// subsequent run, taking down decay for the whole table.
func TestDecay_NeverUnpinsCommittedEpisodes(t *testing.T) {
	ctx := context.Background()
	pool, _ := testsupport.NewDB(t)
	agentID := newAgent(t, pool)
	st := memory.NewStore(pool, fakeworld.HashEmbedder{})

	pinned := insertResolved(t, pool, agentID, "pinned incident", 5)

	// Backdate it so decay drives its salience below the floor too. Without
	// this the second UPDATE never matches the row on salience alone, the
	// expires_at guard is never exercised, and the test passes vacuously.
	// Verified by mutation: removing the guard must fail this test.
	if _, err := pool.Exec(ctx,
		`UPDATE episodes SET created_at = now() - INTERVAL '365 days' WHERE episode_id=$1`,
		pinned); err != nil {
		t.Fatal(err)
	}

	// An ordinary unpinned episode, old enough that decay drives it under the floor.
	v, _ := fakeworld.HashEmbedder{}.Embed(ctx, "unpinned observation")
	var unpinned uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, salience, embedding, created_at, expires_at)
VALUES ('prod', $1, 'unpinned observation', 'n', 0.5, $2,
        now() - INTERVAL '365 days', now() + INTERVAL '30 days')
RETURNING episode_id`, memory.StatusResolved, v).Scan(&unpinned); err != nil {
		t.Fatal(err)
	}

	stats, err := st.Decay(ctx, "prod", memory.DecayOptions{
		HalfLife: 24 * time.Hour, Floor: 0.1, Grace: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.PinnedUntouched < 1 {
		t.Errorf("expected at least one pinned episode, got %d", stats.PinnedUntouched)
	}

	// THE assertion.
	var stillPinned bool
	if err := pool.QueryRow(ctx,
		`SELECT expires_at IS NULL FROM episodes WHERE episode_id=$1`, pinned).Scan(&stillPinned); err != nil {
		t.Fatal(err)
	}
	if !stillPinned {
		t.Fatal("decay un-pinned an episode with a committed action; " +
			"the TTL job would now fail with 23503 on every run")
	}

	// The unpinned one should have faded and been scheduled for expiry.
	var salience float64
	var expiresSoon bool
	if err := pool.QueryRow(ctx,
		`SELECT salience, expires_at < now() + INTERVAL '2 hours' FROM episodes WHERE episode_id=$1`,
		unpinned).Scan(&salience, &expiresSoon); err != nil {
		t.Fatal(err)
	}
	if salience >= 0.5 {
		t.Errorf("salience did not decay: %v", salience)
	}
	if !expiresSoon {
		t.Error("faded unpinned episode was not scheduled for expiry")
	}
}

// TestTimeTravel_ShowsPriorBelief answers "what did the agent believe at time T",
// using MVCC rather than an application-level audit table.
func TestTimeTravel_ShowsPriorBelief(t *testing.T) {
	ctx := context.Background()
	pool, _ := testsupport.NewDB(t)
	agentID := newAgent(t, pool)
	st := memory.NewStore(pool, fakeworld.HashEmbedder{})

	epID := insertResolved(t, pool, agentID, "original belief", 5)

	// Let the write settle, then mark the instant we want to reconstruct.
	time.Sleep(600 * time.Millisecond)
	before := time.Now()
	time.Sleep(600 * time.Millisecond)

	if _, err := pool.Exec(ctx,
		`UPDATE episodes SET narrative='revised after further investigation', salience=0.95
		  WHERE episode_id=$1`, epID); err != nil {
		t.Fatal(err)
	}

	snap := st.AsOf(before)
	past, err := snap.EpisodeAt(ctx, epID)
	if err != nil {
		t.Fatalf("time-travel read failed: %v", err)
	}
	if past.Narrative != "scaled the cluster" {
		t.Errorf("snapshot shows the current narrative, not the historical one: %q", past.Narrative)
	}

	// And the present still reflects the update.
	var now string
	if err := pool.QueryRow(ctx, `SELECT narrative FROM episodes WHERE episode_id=$1`, epID).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if now != "revised after further investigation" {
		t.Errorf("present narrative = %q", now)
	}

	beliefs, err := snap.Beliefs(ctx, "prod", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(beliefs) == 0 {
		t.Fatal("expected beliefs at the snapshot instant")
	}
	for _, b := range beliefs {
		if b.Salience > 0.9 {
			t.Errorf("snapshot leaked a post-snapshot salience change: %+v", b)
		}
	}
}
