// Package testsupport provides a real CockroachDB fixture for integration tests.
//
// These tests run against an actual database rather than a mock, because every
// behaviour worth testing here is a database behaviour: whether the vector index
// is used, whether the TTL job respects a foreign key, whether a serializable
// transaction is atomic. A mock would assert only that the code calls the
// functions the author expected it to call.
package testsupport

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultURL points at the local insecure node used during development. CI and
// the Cloud cluster override it with ANCHOR_TEST_DB_URL.
const DefaultURL = "postgresql://root@localhost:26257/defaultdb?sslmode=disable"

func URL() string {
	if v := os.Getenv("ANCHOR_TEST_DB_URL"); v != "" {
		return v
	}
	return DefaultURL
}

// NewDB creates a fresh database, applies db/schema.sql, and returns a pool
// alongside its connection URL.
//
// Each test gets its own database rather than sharing one with truncation,
// because the reconciler's claim scan is global: a leftover PENDING intent from
// another test would be claimed by this one and quietly change the result.
func NewDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, URL())
	if err != nil {
		t.Skipf("no CockroachDB at %s: %v", URL(), err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("no CockroachDB at %s: %v", URL(), err)
	}

	name := "anchor_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	url := swapDatabase(URL(), name)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}

	for _, stmt := range SplitSQL(schemaSQL(t)) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("applying schema statement:\n%s\nerror: %v", stmt, err)
		}
	}

	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), URL())
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" CASCADE")
	})

	return pool, url
}

// schemaSQL locates db/schema.sql relative to this source file, so tests work
// regardless of which package directory `go test` runs from.
func schemaSQL(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine testsupport source location")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "db", "schema.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema at %s: %v", path, err)
	}
	return string(b)
}

func swapDatabase(url, name string) string {
	slash := strings.LastIndex(url, "/")
	q := strings.Index(url[slash:], "?")
	if q < 0 {
		return url[:slash+1] + name
	}
	return url[:slash+1] + name + url[slash+q:]
}

// SplitSQL splits a script on statement-terminating semicolons, respecting
// single-quoted literals and skipping line comments.
func SplitSQL(s string) []string {
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

// ExplainPlan returns the query plan as a single string, for asserting index use.
func ExplainPlan(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN failed: %v\nquery: %s", err, sql)
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// SeedEpisodes bulk-inserts episodes with SQL-generated random embeddings.
//
// Volume matters for index assertions. CockroachDB will not choose a vector
// index on a tiny or unanalyzed table, so asserting index use against an almost
// empty schema proves nothing. ANALYZE runs afterwards for the same reason.
func SeedEpisodes(t *testing.T, pool *pgxpool.Pool, scopeKey, status string, n int) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, salience, embedding, expires_at)
SELECT $1, $2, 'seeded symptom ' || i::STRING, 'seeded narrative ' || i::STRING,
       0.5,
       (SELECT ARRAY_AGG(random()::FLOAT4) FROM generate_series(1, 1024))::VECTOR(1024),
       now() + INTERVAL '30 days'
  FROM generate_series(1, $3) AS g(i)`, scopeKey, status, n)
	if err != nil {
		t.Fatalf("seeding %d episodes: %v", n, err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE episodes"); err != nil {
		t.Fatalf("analyze episodes: %v", err)
	}
}
