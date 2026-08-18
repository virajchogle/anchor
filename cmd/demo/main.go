// Command demo runs real incidents end to end against live infrastructure.
//
// Nothing here is simulated. Embeddings come from Amazon Bedrock, the action is
// a real ccloud SQL user creation, verification reads the real CockroachDB Cloud
// audit log, and every write lands in the real cluster. It exists so the
// observability panel shows a genuine incident history rather than fixtures.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/bedrock"
	"github.com/virajchogle/anchor/internal/ccloud"
	"github.com/virajchogle/anchor/internal/memory"
	"github.com/virajchogle/anchor/internal/protocol"
	"github.com/virajchogle/anchor/internal/verify"
)

const scope = "prod"

func main() {
	incidents := flag.Int("incidents", 3, "how many incidents of the same class to run")
	cleanup := flag.Bool("cleanup", false, "delete the SQL users this creates")
	flag.Parse()

	ctx := context.Background()
	dbURL := os.Getenv("ANCHOR_DB_URL")
	clusterID := os.Getenv("CCLOUD_CLUSTER_ID")
	if dbURL == "" || clusterID == "" {
		log.Fatal("demo: ANCHOR_DB_URL and CCLOUD_CLUSTER_ID are required")
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("demo: connect: %v", err)
	}
	defer pool.Close()

	embedder, err := bedrock.New(ctx, os.Getenv("AWS_REGION"))
	if err != nil {
		log.Fatalf("demo: bedrock: %v", err)
	}

	client := ccloud.New()
	action := ccloud.CreateSQLUserAction{Client: client, Lookback: 2 * time.Hour}
	reg := verify.NewRegistry()
	verify.MustRegister[ccloud.CreateSQLUserArgs](reg, action)

	mem := memory.NewStore(pool, embedder)
	agentID := ensureAgent(ctx, pool)
	coord := protocol.NewCoordinator(pool, reg, agentID, 2*time.Minute)

	var created []string

	for n := 1; n <= *incidents; n++ {
		symptom := fmt.Sprintf(
			"p99 read latency above SLO on %s, connection pool saturated", clusterID)
		fmt.Printf("\n=== incident %d/%d ===\n%s\n", n, *incidents, symptom)

		// 1. Recall. What has happened like this before?
		prior, err := mem.RecallEpisodes(ctx, memory.Query{
			ScopeKey: scope, Text: symptom, K: 3,
		})
		if err != nil {
			log.Fatalf("recall: %v", err)
		}
		fmt.Printf("recalled %d similar prior incidents\n", len(prior))
		for _, p := range prior {
			fmt.Printf("  similarity=%.3f salience=%.2f  %s\n", p.Similarity, p.Salience, p.Outcome)
		}
		if books, _ := mem.RecallPlaybooks(ctx, scope, symptom, 1); len(books) > 0 {
			fmt.Printf("  playbook: %q (confidence %.3f, from %d episodes)\n",
				books[0].Title, books[0].Confidence, len(books[0].DerivedFrom))
		}

		// 2. Open the episode. It must exist before phase 1, because idem_key
		//    hashes episode_id.
		episodeID := openEpisode(ctx, pool, embedder, symptom)

		// 3. Phase 1: the durable intent, written before anything external.
		args := ccloud.CreateSQLUserArgs{
			ClusterID: clusterID,
			Purpose:   "scoped diagnostic access during a latency incident",
		}
		intent, disp, err := coord.Intend(ctx, episodeID, action.Type(), args)
		if err != nil {
			log.Fatalf("phase 1: %v", err)
		}
		fmt.Printf("phase 1: %s  idem_key=%s…\n", disp, intent.IdemKey[:16])
		if disp != protocol.Owned {
			fmt.Println("not ours to execute; deduplication working as intended")
			continue
		}

		// 4. Phase 2: the real external call.
		receipt, err := reg.Execute(ctx, action.Type(), intent.Args, intent.IdemKey)
		if err != nil {
			log.Fatalf("phase 2: %v", err)
		}
		username := ccloud.UsernameFor(intent.IdemKey)
		created = append(created, username)
		fmt.Printf("phase 2: created SQL user %s\n", username)

		// 5. Verify against ground truth before recording anything as done.
		verdict, err := reg.Verify(ctx, action.Type(), intent.Args, intent.IdemKey, receipt.ExternalRef)
		if err != nil {
			log.Fatalf("verify: %v", err)
		}
		fmt.Printf("verified: %s\n  %s\n", verdict.Disposition, verdict.Reason)
		if verdict.Disposition != verify.Applied {
			fmt.Println("not Applied, leaving the intent for the reconciler")
			continue
		}

		// 6. Phase 3: one transaction.
		narrative := fmt.Sprintf(
			"Provisioned scoped diagnostic user %s to investigate p99 latency. Verified via %s.",
			username, verdict.Reason)
		vec, err := embedder.Embed(ctx, narrative)
		if err != nil {
			log.Fatalf("embedding: %v", err)
		}
		effect, _ := reg.Effect(action.Type(), intent.Args)
		if err := coord.CommitAtomic(ctx, protocol.Commit{
			IdemKey: intent.IdemKey,
			Receipt: &verify.Receipt{ExternalRef: verdict.ExternalRef, Outcome: verdict.Outcome},
			Cluster: effect,
			Memory: protocol.MemoryWrite{
				EpisodeID: episodeID, Narrative: narrative,
				Outcome: "resolved", Embedding: vec,
			},
		}); err != nil {
			log.Fatalf("phase 3: %v", err)
		}
		fmt.Printf("phase 3: committed. intent, world state and memory in one transaction.\n")
	}

	// 7. Consolidate: recurring incidents become a playbook with provenance.
	fmt.Printf("\n=== consolidation ===\n")
	books, err := mem.Consolidate(ctx, scope, memory.ConsolidateOptions{
		MinEpisodes: 3, MaxDistance: 0.25, Archive: true,
	})
	if err != nil {
		log.Fatalf("consolidate: %v", err)
	}
	if len(books) == 0 {
		fmt.Println("no playbook yet; needs 3 sufficiently similar resolved episodes")
	}
	for _, b := range books {
		fmt.Printf("playbook %q confidence=%.3f derived from %d episodes\n",
			b.Title, b.Confidence, len(b.DerivedFrom))
		for _, s := range b.Steps {
			fmt.Printf("  step: %s (observed %d times)\n", s.ActionType, s.Observed)
		}
	}

	if *cleanup {
		fmt.Printf("\ncleaning up %d SQL users\n", len(created))
		for _, u := range created {
			if err := client.DeleteSQLUser(ctx, clusterID, u); err != nil {
				fmt.Printf("  %s: %v\n", u, err)
			}
		}
	} else if len(created) > 0 {
		fmt.Printf("\ncreated %d SQL users; re-run with -cleanup to remove them\n", len(created))
	}
}

func ensureAgent(ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`SELECT agent_id FROM agents WHERE name = 'anchor-demo' LIMIT 1`).Scan(&id)
	if err == nil {
		return id
	}
	id = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (agent_id, name, scope) VALUES ($1,'anchor-demo',ARRAY[$2])`,
		id, scope); err != nil {
		log.Fatalf("demo: creating agent: %v", err)
	}
	return id
}

func openEpisode(ctx context.Context, pool *pgxpool.Pool, e *bedrock.Embedder, symptom string) uuid.UUID {
	vec, err := e.Embed(ctx, symptom)
	if err != nil {
		log.Fatalf("demo: embedding symptom: %v", err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, embedding, expires_at)
VALUES ($1, 'open', $2, 'incident opened', $3, now() + INTERVAL '30 days')
RETURNING episode_id`, scope, symptom, vec).Scan(&id); err != nil {
		log.Fatalf("demo: opening episode: %v", err)
	}
	return id
}
