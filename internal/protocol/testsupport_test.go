package protocol_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/protocol"
)

// seed inserts the agent, cluster, and open episode that an incident starts from.
// The episode exists before phase 1 because idem_key hashes its id.
func seed(t *testing.T, pool *pgxpool.Pool, clusterID string, startNodes int) (agentID, episodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	agentID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (agent_id, name, scope) VALUES ($1, 'anchor-test', ARRAY['prod'])`,
		agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO managed_clusters (cluster_id, scope_key, desired_nodes) VALUES ($1, 'prod', $2)`,
		clusterID, startNodes); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	symptom := "p99 latency above SLO on " + clusterID
	emb, err := fakeworld.HashEmbedder{}.Embed(ctx, symptom)
	if err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	episodeID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO episodes (episode_id, scope_key, status, symptom, narrative, embedding, expires_at)
		 VALUES ($1, 'prod', 'open', $2, 'incident opened', $3, now() + INTERVAL '30 days')`,
		episodeID, symptom, emb); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	return agentID, episodeID
}

func worldPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "world.jsonl")
}

// buildChaosAgent compiles cmd/chaosagent once per test run.
func buildChaosAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "chaosagent")
	run(t, 90*time.Second, "go", "build", "-o", bin, "../../cmd/chaosagent")
	return bin
}

func mustInsertAgent(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (agent_id, name, scope) VALUES ($1, 'anchor-reconciler', ARRAY['prod'])`,
		id); err != nil {
		t.Fatalf("insert reconciler agent: %v", err)
	}
}

func loadIntent(t *testing.T, pool *pgxpool.Pool, idemKey string) protocol.Intent {
	t.Helper()
	var in protocol.Intent
	err := pool.QueryRow(context.Background(),
		`SELECT idem_key, episode_id, action_type, state, coalesce(external_ref,''), attempts
		   FROM action_intents WHERE idem_key = $1`, idemKey).
		Scan(&in.IdemKey, &in.EpisodeID, &in.ActionType, &in.State, &in.ExternalRef, &in.Attempts)
	if err != nil {
		t.Fatalf("load intent %s: %v", idemKey, err)
	}
	return in
}

func assertEpisodeStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, want string) {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM episodes WHERE episode_id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("load episode: %v", err)
	}
	if got != want {
		t.Errorf("episode status = %q, want %q", got, want)
	}
}

// assertEpisodePinned checks the approved TTL fix: an episode with a committed
// action has expires_at NULL and can never be reaped by row-level TTL.
func assertEpisodePinned(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	var pinned bool
	if err := pool.QueryRow(context.Background(),
		`SELECT expires_at IS NULL FROM episodes WHERE episode_id = $1`, id).Scan(&pinned); err != nil {
		t.Fatalf("load episode expiry: %v", err)
	}
	if !pinned {
		t.Error("episode with a committed action is still TTL-eligible; the FK would break the TTL job")
	}
}

func assertCluster(t *testing.T, pool *pgxpool.Pool, clusterID string, wantNodes, wantVersion int) {
	t.Helper()
	var nodes, version int
	if err := pool.QueryRow(context.Background(),
		`SELECT desired_nodes, version FROM managed_clusters WHERE cluster_id = $1`, clusterID).
		Scan(&nodes, &version); err != nil {
		t.Fatalf("load cluster: %v", err)
	}
	if nodes != wantNodes || version != wantVersion {
		t.Errorf("cluster state = (nodes=%d, version=%d), want (nodes=%d, version=%d)",
			nodes, version, wantNodes, wantVersion)
	}
}
