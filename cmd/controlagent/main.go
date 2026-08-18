// Command controlagent is the control architecture's equivalent of chaosagent.
//
// It runs the identical incident against the identical external world, so the
// two designs can be compared under exactly the same failure. The only
// difference is the protocol: this one acts first and records afterwards.
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

	"github.com/virajchogle/anchor/internal/control"
	"github.com/virajchogle/anchor/internal/fakeworld"
)

func main() {
	var (
		dbURL      = flag.String("db", "", "CockroachDB URL")
		worldPath  = flag.String("world", "", "external world op log")
		vectorPath = flag.String("vectors", "", "separate vector store path")
		episodeID  = flag.String("episode", "", "episode UUID")
		clusterID  = flag.String("cluster", "", "cluster id")
		nodes      = flag.Int("nodes", 0, "desired nodes")
		crash      = flag.Bool("crash-after-execute", false, "die after the external call")
		failVector = flag.Bool("fail-vector-write", false, "make the vector store write fail")
	)
	flag.Parse()

	ctx := context.Background()
	db, err := pgxpool.New(ctx, *dbURL)
	if err != nil {
		log.Fatalf("controlagent: connect: %v", err)
	}
	defer db.Close()

	epID := uuid.MustParse(*episodeID)
	world := fakeworld.New(*worldPath)
	vec := control.NewVectorStore(*vectorPath)
	vec.FailNextWrite = *failVector
	agent := &control.Agent{DB: db, Vector: vec}

	// The only guard this architecture has.
	acted, err := agent.HasActedBefore(ctx, epID, "scale_cluster")
	if err != nil {
		log.Fatalf("controlagent: checking prior runs: %v", err)
	}
	if acted {
		fmt.Println("control: already recorded, skipping")
		return
	}

	// Act on the outside world.
	ref := fmt.Sprintf("op-control-%d", time.Now().UnixNano())
	if err := world.Apply(fakeworld.Op{
		ExternalRef: ref,
		ActionType:  "scale_cluster",
		ClusterID:   *clusterID,
		Nodes:       *nodes,
		At:          time.Now().UTC(),
		IdemToken:   "control-" + epID.String(),
	}); err != nil {
		log.Fatalf("controlagent: applying action: %v", err)
	}
	fmt.Printf("control: acted, external_ref=%s\n", ref)

	if *crash {
		// Dies exactly where chaosagent dies, with one difference that decides
		// everything: nothing durable says this action was ever attempted.
		os.Exit(9)
	}

	if err := agent.RecordRun(ctx, epID, "scale_cluster",
		map[string]any{"cluster_id": *clusterID, "nodes": *nodes}, ref); err != nil {
		log.Fatalf("controlagent: recording run: %v", err)
	}
	if err := agent.UpdateCluster(ctx, *clusterID, *nodes); err != nil {
		log.Fatalf("controlagent: updating cluster: %v", err)
	}

	// Second transaction, second store. Can fail alone.
	emb, _ := fakeworld.HashEmbedder{}.Embed(ctx, "scaled "+*clusterID)
	if err := agent.WriteMemory(control.Record{
		EpisodeID: epID.String(),
		Symptom:   "p99 latency above SLO",
		Narrative: fmt.Sprintf("Scaled %s to %d nodes.", *clusterID, *nodes),
		Embedding: emb,
	}); err != nil {
		// Realistic handling: the operational write already committed, so the
		// agent logs the failure and carries on. There is nothing to roll back.
		fmt.Printf("control: MEMORY WRITE FAILED, relational state already committed: %v\n", err)
		os.Exit(0)
	}
	fmt.Println("control: completed")
}
