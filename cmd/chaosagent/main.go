// Command chaosagent runs one full three-phase action and can be told to die
// between phase 2 and phase 3.
//
// It exists so the chaos test kills a real operating-system process rather than
// simulating a crash with a flag inside the test binary. Simulation would prove
// only that the code takes a branch; killing the process proves the protocol
// survives losing everything the process held in memory, which is the claim.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/protocol"
	"github.com/virajchogle/anchor/internal/verify"
)

func main() {
	var (
		dbURL     = flag.String("db", os.Getenv("ANCHOR_TEST_DB_URL"), "CockroachDB connection URL")
		worldPath = flag.String("world", "", "path to the fake external world op log")
		episodeID = flag.String("episode", "", "episode UUID opened by the caller")
		agentID   = flag.String("agent", "", "agent UUID")
		clusterID = flag.String("cluster", "", "cluster id to scale")
		nodes     = flag.Int("nodes", 0, "desired node count")
		crash     = flag.Bool("crash-after-execute", false, "die between phase 2 and phase 3")
		leaseTTL  = flag.Duration("lease", 2*time.Minute, "intent lease duration")
	)
	flag.Parse()

	if *dbURL == "" || *worldPath == "" || *episodeID == "" || *agentID == "" || *clusterID == "" {
		log.Fatal("chaosagent: -db, -world, -episode, -agent and -cluster are required")
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		log.Fatalf("chaosagent: connect: %v", err)
	}
	defer db.Close()

	epID := uuid.MustParse(*episodeID)
	agID := uuid.MustParse(*agentID)

	world := fakeworld.New(*worldPath)
	reg := verify.NewRegistry()
	verify.MustRegister[fakeworld.ScaleArgs](reg, fakeworld.ScaleAction{
		World:             world,
		CrashAfterExecute: *crash,
	})

	coord := protocol.NewCoordinator(db, reg, agID, *leaseTTL)
	args := fakeworld.ScaleArgs{ClusterID: *clusterID, Nodes: *nodes, Reason: "high p99 latency"}

	// Phase 1: record the durable intent before touching the outside world.
	intent, disp, err := coord.Intend(ctx, epID, "scale_cluster", args)
	if err != nil {
		log.Fatalf("chaosagent: phase 1: %v", err)
	}
	fmt.Printf("phase1 disposition=%s idem_key=%s\n", disp, intent.IdemKey)

	if disp != protocol.Owned {
		// Not ours to execute. This is the deduplication path working.
		fmt.Printf("not executing: %s\n", disp)
		return
	}

	// Phase 2: the external call. With -crash-after-execute this never returns.
	receipt, err := reg.Execute(ctx, "scale_cluster", intent.Args, intent.IdemKey)
	if err != nil {
		log.Fatalf("chaosagent: phase 2: %v", err)
	}
	fmt.Printf("phase2 external_ref=%s\n", receipt.ExternalRef)

	// Phase 3: intent resolution, world state, and memory in one transaction.
	narrative := fmt.Sprintf("Scaled %s to %d nodes in response to high p99 latency.", *clusterID, *nodes)
	emb, err := fakeworld.HashEmbedder{}.Embed(ctx, narrative)
	if err != nil {
		log.Fatalf("chaosagent: embedding: %v", err)
	}

	effect, err := reg.Effect("scale_cluster", intent.Args)
	if err != nil {
		log.Fatalf("chaosagent: effect: %v", err)
	}

	if err := coord.CommitAtomic(ctx, protocol.Commit{
		IdemKey: intent.IdemKey,
		Receipt: receipt,
		Cluster: effect,
		Memory: protocol.MemoryWrite{
			EpisodeID: epID,
			Narrative: narrative,
			Outcome:   "resolved",
			Embedding: emb,
		},
	}); err != nil {
		log.Fatalf("chaosagent: phase 3: %v", err)
	}

	out, _ := json.Marshal(map[string]string{"idem_key": intent.IdemKey, "external_ref": receipt.ExternalRef})
	fmt.Printf("phase3 committed %s\n", out)
}
