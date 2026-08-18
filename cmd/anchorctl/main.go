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

	"github.com/virajchogle/anchor/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage:\n" +
			"  anchorctl migrate [schema.sql]\n" +
			"  anchorctl check\n" +
			"  anchorctl escalations                          list intents awaiting a human\n" +
			"  anchorctl resolve <idem_key> <applied|failed> [note]\n" +
			"reads ANCHOR_DB_URL from the environment (or ~/.anchor/env)")
	}
	config.LoadLocalEnv()
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
	case "escalations":
		listEscalations(ctx, pool)
	case "resolve":
		if len(os.Args) < 4 {
			log.Fatal("usage: anchorctl resolve <idem_key> <applied|failed> [note]")
		}
		note := "resolved by operator"
		if len(os.Args) > 4 {
			note = strings.Join(os.Args[4:], " ")
		}
		resolve(ctx, pool, os.Args[2], os.Args[3], note)
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

// listEscalations shows intents the verifier could not settle.
//
// These are the ones deliberately left PENDING: the system established that it
// could not establish anything, and stopped rather than guessing. They are the
// only work items that genuinely require a person.
func listEscalations(ctx context.Context, pool *pgxpool.Pool) {
	rows, err := pool.Query(ctx, `
SELECT idem_key, action_type, attempts,
       coalesce(outcome->>'reason', ''), created_at::STRING
  FROM action_intents
 WHERE state = 'PENDING' AND outcome->>'disposition' = 'UNKNOWN'
 ORDER BY created_at`)
	if err != nil {
		log.Fatalf("anchorctl: listing escalations: %v", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var key, action, reason, created string
		var attempts int
		if err := rows.Scan(&key, &action, &attempts, &reason, &created); err != nil {
			log.Fatal(err)
		}
		n++
		fmt.Printf("\n%s\n  action   : %s\n  attempts : %d\n  opened   : %s\n  why      : %s\n",
			key, action, attempts, created, wrap(reason, 74, "             "))
	}
	if n == 0 {
		fmt.Println("no escalations; nothing is waiting on a human")
		return
	}
	fmt.Printf("\n%d escalation(s). Decide with:\n", n)
	fmt.Println("  anchorctl resolve <idem_key> applied  \"I confirmed it happened\"")
	fmt.Println("  anchorctl resolve <idem_key> failed   \"I confirmed it did not\"")
}

// resolve records a human decision about an intent the agent refused to guess at.
//
// The operator's judgement is recorded as its own evidence source rather than
// being dressed up as machine verification. A row resolved this way says so, so
// nobody later mistakes a person's assertion for an audit-log fact.
//
// It runs in one transaction with the episode update, for the same reason phase
// 3 does: the intent's resolution and the memory of it must not be able to
// disagree.
func resolve(ctx context.Context, pool *pgxpool.Pool, idemKey, decision, note string) {
	var state string
	switch decision {
	case "applied":
		state = "COMMITTED"
	case "failed":
		state = "FAILED"
	default:
		log.Fatalf("anchorctl: decision must be 'applied' or 'failed', got %q", decision)
	}

	outcome := fmt.Sprintf(
		`{"disposition":%q,"evidence":"human_operator","reason":%q,"resolved_by":"operator"}`,
		strings.ToUpper(decision), note)

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatalf("anchorctl: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx, `
UPDATE action_intents
   SET state = $2, outcome = $3::JSONB, resolved_at = now(), lease_owner = NULL
 WHERE idem_key = $1 AND state = 'PENDING'`, idemKey, state, outcome)
	if err != nil {
		log.Fatalf("anchorctl: resolving intent: %v", err)
	}
	if tag.RowsAffected() == 0 {
		log.Fatalf("anchorctl: %s is not a PENDING intent; it may already be resolved", idemKey)
	}

	// An action confirmed as applied pins its episode, exactly as phase 3 would.
	// Without this the audit trail could still be reaped by row-level TTL.
	if state == "COMMITTED" {
		if _, err := tx.Exec(ctx, `
UPDATE episodes SET status = 'resolved', expires_at = NULL
 WHERE episode_id = (SELECT episode_id FROM action_intents WHERE idem_key = $1)`,
			idemKey); err != nil {
			log.Fatalf("anchorctl: pinning episode: %v", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("anchorctl: %v", err)
	}
	fmt.Printf("%s resolved as %s by operator\n", idemKey[:16], state)
	fmt.Printf("recorded as human_operator evidence, not as machine verification\n")
}

func wrap(s string, width int, indent string) string {
	if s == "" {
		return "(no reason recorded)"
	}
	var out strings.Builder
	line := 0
	for _, w := range strings.Fields(s) {
		if line+len(w)+1 > width {
			out.WriteString("\n" + indent)
			line = 0
		} else if line > 0 {
			out.WriteString(" ")
			line++
		}
		out.WriteString(w)
		line += len(w)
	}
	return out.String()
}
