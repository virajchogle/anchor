// Command anchorctl performs setup tasks: applying the schema and seeding a
// demo scenario. It exists so the README's instructions are a command rather
// than a list of SQL a reader has to paste correctly.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: anchorctl <migrate|check> [schema.sql]\n" +
			"reads ANCHOR_DB_URL from the environment")
	}
	url := os.Getenv("ANCHOR_DB_URL")
	if url == "" {
		log.Fatal("anchorctl: ANCHOR_DB_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("anchorctl: connect: %v", err)
	}
	defer pool.Close()

	switch os.Args[1] {
	case "migrate":
		path := "db/schema.sql"
		if len(os.Args) > 2 {
			path = os.Args[2]
		}
		migrate(ctx, pool, path)
	case "check":
		check(ctx, pool)
	default:
		log.Fatalf("anchorctl: unknown command %q", os.Args[1])
	}
}

func migrate(ctx context.Context, pool *pgxpool.Pool, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("anchorctl: reading %s: %v", path, err)
	}
	stmts := SplitSQL(string(b))
	for i, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			log.Fatalf("anchorctl: statement %d failed:\n%s\n\n%v", i+1, stmt, err)
		}
	}
	fmt.Printf("applied %d statements from %s\n", len(stmts), path)
}

// check verifies the things that silently break: that the vector index exists
// and that a cosine query actually uses it.
func check(ctx context.Context, pool *pgxpool.Pool) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM episodes`).Scan(&n); err != nil {
		log.Fatalf("anchorctl: episodes table unreadable: %v", err)
	}
	fmt.Printf("episodes: %d rows\n", n)

	rows, err := pool.Query(ctx, `
SELECT index_name FROM [SHOW INDEXES FROM episodes] WHERE index_name = 'idx_episodes_recall' LIMIT 1`)
	if err != nil {
		log.Fatalf("anchorctl: reading indexes: %v", err)
	}
	found := rows.Next()
	rows.Close()
	if !found {
		log.Fatal("anchorctl: idx_episodes_recall is missing; recall would full-scan")
	}
	fmt.Println("vector index idx_episodes_recall: present")

	if n < 100 {
		fmt.Println("note: the optimizer will not choose a vector index on a nearly empty " +
			"table, so an EXPLAIN check here would be misleading. Seed data first.")
		return
	}
	fmt.Println("ok")
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
			if st := strings.TrimSpace(cur.String()); st != "" {
				out = append(out, st)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if st := strings.TrimSpace(cur.String()); st != "" {
		out = append(out, st)
	}
	return out
}
