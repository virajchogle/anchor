// Command bench measures Anchor and its control, and writes docs/RESULTS.md.
//
// Every number in the documentation comes from this program. Nothing is
// estimated, extrapolated, or rounded up from a single run.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/protocol"
	"github.com/virajchogle/anchor/internal/store"
	"github.com/virajchogle/anchor/internal/verify"
)

func main() {
	var (
		dbURL   = flag.String("db", os.Getenv("ANCHOR_BENCH_DB_URL"), "CockroachDB URL")
		writes  = flag.Int("writes", 200, "memory writes for the throughput measurement")
		reads   = flag.Int("reads", 200, "recall queries for the latency measurement")
		agents  = flag.Int("agents", 16, "concurrent agents contending on one scope")
		corpus  = flag.Int("corpus", 2000, "episodes seeded before measuring recall")
		outPath = flag.String("out", "docs/RESULTS.md", "output path")
	)
	flag.Parse()
	if *dbURL == "" {
		log.Fatal("bench: -db or ANCHOR_BENCH_DB_URL is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		log.Fatalf("bench: connect: %v", err)
	}
	defer pool.Close()

	env := describeEnv(ctx, pool)
	log.Printf("bench: %s", env.version)

	r := &results{Env: env, Started: time.Now()}
	emb := fakeworld.HashEmbedder{}

	log.Printf("seeding %d episodes", *corpus)
	seedCorpus(ctx, pool, *corpus)

	log.Printf("measuring memory write throughput (%d writes)", *writes)
	r.Write = measureWrites(ctx, pool, emb, *writes)

	log.Printf("measuring recall latency (%d queries over %d episodes)", *reads, *corpus)
	r.Recall = measureRecall(ctx, pool, emb, *reads)

	log.Printf("measuring contention with %d concurrent agents", *agents)
	r.Contention = measureContention(ctx, pool, emb, *agents)

	r.Finished = time.Now()
	if err := os.WriteFile(*outPath, []byte(r.render()), 0o644); err != nil {
		log.Fatalf("bench: writing %s: %v", *outPath, err)
	}
	log.Printf("bench: wrote %s", *outPath)
}

type environment struct{ version, plan, region string }

func describeEnv(ctx context.Context, pool *pgxpool.Pool) environment {
	var e environment
	_ = pool.QueryRow(ctx, "SELECT version()").Scan(&e.version)
	if i := strings.Index(e.version, " ("); i > 0 {
		e.version = e.version[:i]
	}
	_ = pool.QueryRow(ctx, "SELECT current_database()").Scan(&e.plan)
	_ = pool.QueryRow(ctx, "SELECT region FROM [SHOW REGIONS] LIMIT 1").Scan(&e.region)
	return e
}

func seedCorpus(ctx context.Context, pool *pgxpool.Pool, n int) {
	if n <= 0 {
		return
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, salience, embedding, expires_at)
SELECT 'bench', 'resolved', 'seeded symptom ' || i::STRING, 'seeded narrative ' || i::STRING, 0.5,
       (SELECT ARRAY_AGG(random()::FLOAT4) FROM generate_series(1,1024))::VECTOR(1024),
       now() + INTERVAL '30 days'
  FROM generate_series(1, $1) AS g(i)`, n); err != nil {
		log.Fatalf("bench: seeding corpus: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE episodes"); err != nil {
		log.Fatalf("bench: analyze: %v", err)
	}
}

type latency struct {
	N               int
	P50, P95, P99   time.Duration
	Min, Max, Total time.Duration
}

func summarize(samples []time.Duration) latency {
	if len(samples) == 0 {
		return latency{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var total time.Duration
	for _, s := range samples {
		total += s
	}
	pct := func(p float64) time.Duration {
		idx := int(float64(len(samples)-1) * p)
		return samples[idx]
	}
	return latency{
		N: len(samples), P50: pct(0.50), P95: pct(0.95), P99: pct(0.99),
		Min: samples[0], Max: samples[len(samples)-1], Total: total,
	}
}

func (l latency) throughput() float64 {
	if l.Total == 0 {
		return 0
	}
	return float64(l.N) / l.Total.Seconds()
}

// measureWrites times the full phase 3 transaction: intent resolution, world
// state, and the memory write with its embedding, all in one transaction. It is
// deliberately not a bare INSERT, because the claim being measured is that the
// atomic unit is affordable.
func measureWrites(ctx context.Context, pool *pgxpool.Pool, emb fakeworld.HashEmbedder, n int) latency {
	agentID := mustAgent(ctx, pool)
	reg := verify.NewRegistry()
	verify.MustRegister[fakeworld.ScaleArgs](reg, fakeworld.ScaleAction{World: fakeworld.New(os.DevNull)})
	coord := protocol.NewCoordinator(pool, reg, agentID, 2*time.Minute)

	mustCluster(ctx, pool, "bench-writes")

	samples := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		epID := mustEpisode(ctx, pool, emb, fmt.Sprintf("write bench %d", i))
		args := fakeworld.ScaleArgs{ClusterID: "bench-writes", Nodes: i%7 + 3}
		intent, disp, err := coord.Intend(ctx, epID, "scale_cluster", args)
		if err != nil || disp != protocol.Owned {
			log.Fatalf("bench: phase 1 (%v): %v", disp, err)
		}
		vec, _ := emb.Embed(ctx, fmt.Sprintf("resolved write bench %d", i))
		nodes := args.Nodes

		start := time.Now()
		if err := coord.CommitAtomic(ctx, protocol.Commit{
			IdemKey: intent.IdemKey,
			Receipt: &verify.Receipt{ExternalRef: "bench", Outcome: []byte(`{"bench":true}`)},
			Cluster: &verify.WorldEffect{ClusterID: "bench-writes", DesiredNodes: &nodes, LastAction: "scale_cluster"},
			Memory:  protocol.MemoryWrite{EpisodeID: epID, Narrative: "n", Outcome: "resolved", Embedding: vec},
		}); err != nil {
			log.Fatalf("bench: phase 3: %v", err)
		}
		samples = append(samples, time.Since(start))
	}
	return summarize(samples)
}

func measureRecall(ctx context.Context, pool *pgxpool.Pool, emb fakeworld.HashEmbedder, n int) latency {
	samples := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		vec, _ := emb.Embed(ctx, fmt.Sprintf("p99 latency spike variant %d", i))
		start := time.Now()
		rows, err := pool.Query(ctx, `
SELECT episode_id FROM episodes
 WHERE scope_key = 'bench' AND status IN ('resolved','archived')
 ORDER BY embedding <=> $1
 LIMIT 20`, vec)
		if err != nil {
			log.Fatalf("bench: recall: %v", err)
		}
		for rows.Next() {
		}
		rows.Close()
		samples = append(samples, time.Since(start))
	}
	return summarize(samples)
}

type contention struct {
	Agents            int
	Attempts          int
	Committed         int64
	Deduplicated      int64
	SerializationRetr int64
	LostUpdates       int
	ClusterVersion    int
	Elapsed           time.Duration
}

// measureContention runs many agents at the same logical action on one scope.
//
// Two properties are checked, not just timed. Every agent that is told it owns
// the intent must be the only one that acts, and the cluster version must equal
// the number of committed actions. A mismatch is a lost update.
func measureContention(ctx context.Context, pool *pgxpool.Pool, emb fakeworld.HashEmbedder, agents int) contention {
	mustCluster(ctx, pool, "bench-contend")
	epID := mustEpisode(ctx, pool, emb, "contended incident")

	var committed, deduped, retries int64
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agentID := mustAgent(ctx, pool)
			reg := verify.NewRegistry()
			verify.MustRegister[fakeworld.ScaleArgs](reg, fakeworld.ScaleAction{World: fakeworld.New(os.DevNull)})
			coord := protocol.NewCoordinator(pool, reg, agentID, 2*time.Minute)

			// Every agent proposes the identical logical action, so all of them
			// derive the same idempotency key and exactly one may proceed.
			args := fakeworld.ScaleArgs{ClusterID: "bench-contend", Nodes: 9}
			intent, disp, err := coord.Intend(ctx, epID, "scale_cluster", args)
			if err != nil {
				return
			}
			if disp != protocol.Owned {
				atomic.AddInt64(&deduped, 1)
				return
			}
			vec, _ := emb.Embed(ctx, "contended resolution")
			nodes := 9
			if err := coord.CommitAtomic(ctx, protocol.Commit{
				IdemKey: intent.IdemKey,
				Receipt: &verify.Receipt{ExternalRef: "bench", Outcome: []byte(`{"bench":true}`)},
				Cluster: &verify.WorldEffect{ClusterID: "bench-contend", DesiredNodes: &nodes, LastAction: "scale_cluster"},
				Memory:  protocol.MemoryWrite{EpisodeID: epID, Narrative: "n", Outcome: "resolved", Embedding: vec},
			}); err != nil {
				var pgErr *pgconn.PgError
				if ok := asPg(err, &pgErr); ok && pgErr.Code == "40001" {
					atomic.AddInt64(&retries, 1)
				}
				return
			}
			atomic.AddInt64(&committed, 1)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	var version int
	_ = pool.QueryRow(ctx, `SELECT version FROM managed_clusters WHERE cluster_id='bench-contend'`).Scan(&version)

	return contention{
		Agents: agents, Attempts: agents,
		Committed: committed, Deduplicated: deduped, SerializationRetr: retries,
		ClusterVersion: version,
		LostUpdates:    int(committed) - version,
		Elapsed:        elapsed,
	}
}

func asPg(err error, target **pgconn.PgError) bool {
	for err != nil {
		if pe, ok := err.(*pgconn.PgError); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func mustAgent(ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (agent_id, name, scope) VALUES ($1,'bench',ARRAY['bench'])`, id); err != nil {
		log.Fatalf("bench: agent: %v", err)
	}
	return id
}

func mustCluster(ctx context.Context, pool *pgxpool.Pool, id string) {
	if _, err := pool.Exec(ctx,
		`UPSERT INTO managed_clusters (cluster_id, scope_key, desired_nodes) VALUES ($1,'bench',3)`, id); err != nil {
		log.Fatalf("bench: cluster: %v", err)
	}
}

func mustEpisode(ctx context.Context, pool *pgxpool.Pool, emb fakeworld.HashEmbedder, symptom string) uuid.UUID {
	vec, _ := emb.Embed(ctx, symptom)
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO episodes (scope_key, status, symptom, narrative, embedding, expires_at)
VALUES ('bench','open',$1,'opened',$2, now() + INTERVAL '30 days')
RETURNING episode_id`, symptom, store.Vector(vec)).Scan(&id); err != nil {
		log.Fatalf("bench: episode: %v", err)
	}
	return id
}

type results struct {
	Env               environment
	Started, Finished time.Time
	Write, Recall     latency
	Contention        contention
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

func (r *results) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmark results\n\n")
	fmt.Fprintf(&b, "Generated by `cmd/bench`. Every number here is measured. ")
	fmt.Fprintf(&b, "Nothing in this file is estimated or extrapolated.\n\n")
	fmt.Fprintf(&b, "- Run at: %s\n", r.Started.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Duration: %s\n", r.Finished.Sub(r.Started).Round(time.Second))
	fmt.Fprintf(&b, "- Server: %s\n", r.Env.version)
	if r.Env.region != "" {
		fmt.Fprintf(&b, "- Region: %s\n", r.Env.region)
	}
	fmt.Fprintf(&b, "\n## Memory write (full phase 3 transaction)\n\n")
	fmt.Fprintf(&b, "Intent resolution, world-state mutation, and the 1024-dimension embedding ")
	fmt.Fprintf(&b, "committed in one serializable transaction.\n\n")
	fmt.Fprintf(&b, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Transactions | %d |\n", r.Write.N)
	fmt.Fprintf(&b, "| Throughput | %.1f txn/s |\n", r.Write.throughput())
	fmt.Fprintf(&b, "| p50 | %s |\n| p95 | %s |\n| p99 | %s |\n", ms(r.Write.P50), ms(r.Write.P95), ms(r.Write.P99))
	fmt.Fprintf(&b, "| min / max | %s / %s |\n", ms(r.Write.Min), ms(r.Write.Max))

	fmt.Fprintf(&b, "\n## Recall latency (vector index)\n\n")
	fmt.Fprintf(&b, "Cosine search with both prefix columns constrained, top 20.\n\n")
	fmt.Fprintf(&b, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Queries | %d |\n", r.Recall.N)
	fmt.Fprintf(&b, "| p50 | %s |\n| p95 | %s |\n| p99 | %s |\n", ms(r.Recall.P50), ms(r.Recall.P95), ms(r.Recall.P99))
	fmt.Fprintf(&b, "| min / max | %s / %s |\n", ms(r.Recall.Min), ms(r.Recall.Max))

	c := r.Contention
	fmt.Fprintf(&b, "\n## Contention: %d concurrent agents, one logical action\n\n", c.Agents)
	fmt.Fprintf(&b, "Every agent proposes the identical action against the same episode, so all ")
	fmt.Fprintf(&b, "derive the same idempotency key. Exactly one may act.\n\n")
	fmt.Fprintf(&b, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Agents | %d |\n", c.Agents)
	fmt.Fprintf(&b, "| Committed | %d |\n", c.Committed)
	fmt.Fprintf(&b, "| Deduplicated by phase 1 | %d |\n", c.Deduplicated)
	fmt.Fprintf(&b, "| Serialization retries (40001) | %d |\n", c.SerializationRetr)
	fmt.Fprintf(&b, "| Cluster version after run | %d |\n", c.ClusterVersion)
	fmt.Fprintf(&b, "| **Lost updates** | **%d** |\n", c.LostUpdates)
	fmt.Fprintf(&b, "| Wall clock | %s |\n", c.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&b, "\nCommitted actions must equal the cluster version. A non-zero lost-update ")
	fmt.Fprintf(&b, "count would mean a committed action left no trace in world state.\n")

	fmt.Fprintf(&b, "\n## Control architecture\n\n")
	fmt.Fprintf(&b, "The control is measured by test rather than by timing, because its failures ")
	fmt.Fprintf(&b, "are categorical rather than statistical. See `internal/control`:\n\n")
	fmt.Fprintf(&b, "| Scenario | Anchor | Control |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Crash between action and commit | 1 external operation | **2 external operations** |\n")
	fmt.Fprintf(&b, "| Memory write fails, no crash | impossible: same transaction | **world changed, memory empty** |\n")
	fmt.Fprintf(&b, "| No failure at all | 1 operation, memory written | 1 operation, memory written |\n")
	return b.String()
}
