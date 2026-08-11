package memory_test

import (
	"context"
	"strings"
	"testing"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/memory"
	"github.com/virajchogle/anchor/internal/testsupport"
)

// seedVolume is chosen to be past the point where the optimizer stops preferring
// a full scan. Asserting index use on a near-empty table proves nothing, which
// is a documented trap of CockroachDB vector indexes.
const seedVolume = 2000

// TestRecall_UsesVectorIndex is the assertion the brief asks for: prove via
// EXPLAIN that the index is actually used and not silently bypassed.
func TestRecall_UsesVectorIndex(t *testing.T) {
	pool, _ := testsupport.NewDB(t)
	testsupport.SeedEpisodes(t, pool, "prod", memory.StatusResolved, seedVolume)

	vec, err := fakeworld.HashEmbedder{}.Embed(context.Background(), "p99 latency spike")
	if err != nil {
		t.Fatal(err)
	}

	plan := testsupport.ExplainPlan(t, pool, `
SELECT episode_id FROM episodes
 WHERE scope_key = $2 AND status IN ($3, $4)
 ORDER BY embedding <=> $1
 LIMIT 5`, vec, "prod", memory.StatusResolved, memory.StatusArchived)

	t.Logf("plan:\n%s", plan)

	if !strings.Contains(plan, "vector search") {
		t.Error("query did not use a vector search node")
	}
	if !strings.Contains(plan, "idx_episodes_recall") {
		t.Error("query did not use idx_episodes_recall")
	}
	if strings.Contains(plan, "FULL SCAN") {
		t.Error("query degraded to a full scan")
	}
	// Both status values should become their own prefix span.
	if !strings.Contains(plan, "prefix spans") {
		t.Error("prefix columns were not pushed into the index scan")
	}
}

// TestRecall_IndexTraps pins the two documented failure modes as regressions.
// Both degrade silently, so a test is the only thing standing between us and a
// full scan in production.
func TestRecall_IndexTraps(t *testing.T) {
	pool, _ := testsupport.NewDB(t)
	testsupport.SeedEpisodes(t, pool, "prod", memory.StatusResolved, seedVolume)

	vec, _ := fakeworld.HashEmbedder{}.Embed(context.Background(), "p99 latency spike")

	t.Run("L2 operator against a cosine index falls back", func(t *testing.T) {
		plan := testsupport.ExplainPlan(t, pool, `
SELECT episode_id FROM episodes
 WHERE scope_key = $2 AND status = $3
 ORDER BY embedding <-> $1
 LIMIT 5`, vec, "prod", memory.StatusResolved)

		if strings.Contains(plan, "vector search") {
			t.Errorf("unexpected: <-> now uses a vector_cosine_ops index.\n"+
				"If CockroachDB changed this, recall could stop being operator-dependent.\n%s", plan)
		}
	})

	t.Run("non-prefix predicate disqualifies the index", func(t *testing.T) {
		plan := testsupport.ExplainPlan(t, pool, `
SELECT episode_id FROM episodes
 WHERE scope_key = $2 AND status = $3 AND salience > 0.4
 ORDER BY embedding <=> $1
 LIMIT 5`, vec, "prod", memory.StatusResolved)

		if strings.Contains(plan, "vector search") {
			t.Errorf("unexpected: a non-prefix predicate no longer disqualifies the index.\n"+
				"If so, memory.rankAndTrim could push salience into SQL.\n%s", plan)
		}
	})
}

// TestRecall_AppliesPostRetrievalFilters checks that the filters which cannot
// live in SQL are actually applied in Go.
func TestRecall_AppliesPostRetrievalFilters(t *testing.T) {
	ctx := context.Background()
	pool, _ := testsupport.NewDB(t)
	emb := fakeworld.HashEmbedder{}
	st := memory.NewStore(pool, emb)

	insert := func(symptom string, salience float64) {
		t.Helper()
		v, err := emb.Embed(ctx, symptom)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, salience, embedding, expires_at)
VALUES ('prod', $1, $2, 'narrative', $3, $4, now() + INTERVAL '30 days')`,
			memory.StatusResolved, symptom, salience, v); err != nil {
			t.Fatal(err)
		}
	}

	insert("p99 latency spike on the orders cluster", 0.9)
	insert("p99 latency spike on the orders cluster", 0.05) // below the floor
	insert("disk usage climbing on the billing cluster", 0.8)

	got, err := st.RecallEpisodes(ctx, memory.Query{
		ScopeKey:    "prod",
		Text:        "p99 latency spike on the orders cluster",
		K:           5,
		MinSalience: 0.1,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range got {
		if e.Salience < 0.1 {
			t.Errorf("episode below the salience floor was returned: %+v", e)
		}
	}
	if len(got) == 0 {
		t.Fatal("expected at least one recalled episode")
	}
	// The exact-match episode should rank first.
	if !strings.Contains(got[0].Symptom, "p99 latency spike") {
		t.Errorf("expected the semantically closest episode first, got %q", got[0].Symptom)
	}
	// Ranking components must be populated for the observability panel.
	if got[0].Similarity == 0 || got[0].Score == 0 {
		t.Errorf("ranking components not populated: %+v", got[0])
	}
}

// TestRecall_ScopeIsolation checks that memory from another scope is never
// returned, which is the retrieval half of the security model.
func TestRecall_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	pool, _ := testsupport.NewDB(t)
	emb := fakeworld.HashEmbedder{}
	st := memory.NewStore(pool, emb)

	v, _ := emb.Embed(ctx, "secret staging incident")
	if _, err := pool.Exec(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, salience, embedding, expires_at)
VALUES ('staging', $1, 'secret staging incident', 'n', 0.9, $2, now() + INTERVAL '30 days')`,
		memory.StatusResolved, v); err != nil {
		t.Fatal(err)
	}

	got, err := st.RecallEpisodes(ctx, memory.Query{
		ScopeKey: "prod",
		Text:     "secret staging incident",
		K:        5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("recall leaked %d episodes across scopes: %+v", len(got), got)
	}
}
