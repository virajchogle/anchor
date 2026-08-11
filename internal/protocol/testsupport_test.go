package protocol_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/protocol"
)

// defaultTestDB points at the local insecure node used during development. CI
// and the Cloud cluster override it with ANCHOR_TEST_DB_URL.
const defaultTestDB = "postgresql://root@localhost:26257/defaultdb?sslmode=disable"

func testDBURL() string {
	if v := os.Getenv("ANCHOR_TEST_DB_URL"); v != "" {
		return v
	}
	return defaultTestDB
}

// newTestDB creates a fresh database, applies db/schema.sql, and returns a pool.
//
// Each test gets its own database rather than truncating shared tables, because
// the reconciler's claim scan is global: leftover PENDING intents from another
// test would be claimed by this one and quietly change the result.
func newTestDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, testDBURL())
	if err != nil {
		t.Skipf("no CockroachDB at %s: %v", testDBURL(), err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("no CockroachDB at %s: %v", testDBURL(), err)
	}

	dbName := "anchor_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	url := swapDatabase(testDBURL(), dbName)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}

	schema, err := os.ReadFile(filepath.Join("..", "..", "db", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	// Statements are applied one at a time so a failure names the statement.
	for _, stmt := range splitSQL(string(schema)) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("applying schema statement:\n%s\nerror: %v", stmt, err)
		}
	}

	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), testDBURL())
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" CASCADE")
	})

	return pool, url
}

func swapDatabase(url, dbName string) string {
	slash := strings.LastIndex(url, "/")
	q := strings.Index(url[slash:], "?")
	if q < 0 {
		return url[:slash+1] + dbName
	}
	return url[:slash+1] + dbName + url[slash+q:]
}

// splitSQL splits on semicolons that terminate a statement, respecting the
// single-quoted literals that appear in the TTL and CHECK clauses.
func splitSQL(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == '-' && i+1 < len(s) && s[i+1] == '-' && !inQuote:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			cur.WriteByte('\n')
		case c == ';' && !inQuote:
			if stmt := strings.TrimSpace(cur.String()); stmt != "" {
				out = append(out, stmt)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

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
