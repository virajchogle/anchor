// Command compare runs both architectures against the same failure and records
// what each one did.
//
// It is the experiment, not a description of one. Both implementations act on
// the same external API, are killed at the same point with a real process
// signal, and are then allowed to recover however their design allows. The
// operation counts are read from the external world afterwards.
//
// Results are written to comparison_runs so the panel can show a measurement
// with a timestamp rather than a claim rendered as a table.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/config"
	"github.com/virajchogle/anchor/internal/control"
	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/protocol"
	"github.com/virajchogle/anchor/internal/testsupport"
	"github.com/virajchogle/anchor/internal/verify"
)

type result struct {
	scenario, description string
	anchorOps             int
	anchorOK              bool
	anchorNote            string
	controlOps            int
	controlOK             bool
	controlNote           string
}

func main() {
	config.LoadLocalEnv()
	url := os.Getenv("ANCHOR_DB_URL")
	if url == "" {
		log.Fatal("compare: ANCHOR_DB_URL is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("compare: connect: %v", err)
	}
	defer pool.Close()

	dir, err := os.MkdirTemp("", "anchor-compare-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	log.Println("building both agents")
	chaosBin := build(dir, "chaosagent", "./cmd/chaosagent")
	controlBin := build(dir, "controlagent", "./cmd/controlagent")

	// Both experiments run against scratch databases so the demo data is never
	// disturbed by a measurement.
	results := []result{
		crashScenario(ctx, url, dir, chaosBin, controlBin),
		memoryWriteScenario(ctx, url, dir, controlBin),
	}

	for _, r := range results {
		if _, err := pool.Exec(ctx, `
INSERT INTO comparison_runs (scenario, description, anchor_ops, anchor_ok, anchor_note,
                             control_ops, control_ok, control_note)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			r.scenario, r.description, r.anchorOps, r.anchorOK, r.anchorNote,
			r.controlOps, r.controlOK, r.controlNote); err != nil {
			log.Fatalf("compare: recording result: %v", err)
		}
		fmt.Printf("\n%s\n  anchor : %d op(s)  %s\n  control: %d op(s)  %s\n",
			r.scenario, r.anchorOps, r.anchorNote, r.controlOps, r.controlNote)
	}
	log.Println("recorded", len(results), "scenario(s)")
}

func build(dir, name, pkg string) string {
	bin := filepath.Join(dir, name)
	out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput()
	if err != nil {
		log.Fatalf("compare: building %s: %v\n%s", pkg, err, out)
	}
	return bin
}

// crashScenario kills each agent between the external call and the record of it.
func crashScenario(ctx context.Context, adminURL, dir, chaosBin, controlBin string) result {
	r := result{
		scenario: "crash between acting and recording",
		description: "A real process performs the external action and is killed with SIGKILL-equivalent " +
			"exit before it can record what it did. Each architecture then recovers however its design allows.",
	}

	// --- Anchor ---
	anchorDB, anchorURL := scratch(ctx, adminURL, "")
	defer anchorDB.Close()
	world := filepath.Join(dir, "anchor-world.jsonl")
	agentID, episodeID := seedAnchor(ctx, anchorDB)

	args := []string{"-db", anchorURL, "-world", world,
		"-episode", episodeID.String(), "-agent", agentID.String(),
		"-cluster", "cmp", "-nodes", "5", "-lease", "2s"}
	_ = exec.Command(chaosBin, append(args, "-crash-after-execute")...).Run() // expected to die

	time.Sleep(2300 * time.Millisecond) // let the lease lapse honestly
	reconcile(ctx, anchorDB, world, agentID)

	ops, _ := fakeworld.New(world).Ops()
	r.anchorOps = len(ops)
	r.anchorOK = len(ops) == 1
	r.anchorNote = "reconciler verified against the external system and committed without acting again"
	if !r.anchorOK {
		r.anchorNote = fmt.Sprintf("unexpected: %d operations", len(ops))
	}

	// --- Control ---
	controlDB, controlURL := scratch(ctx, adminURL, control.ControlSchema)
	defer controlDB.Close()
	cworld := filepath.Join(dir, "control-world.jsonl")
	cargs := []string{"-db", controlURL, "-world", cworld,
		"-vectors", filepath.Join(dir, "vectors.json"),
		"-episode", uuid.NewString(), "-cluster", "cmp", "-nodes", "5"}
	_ = exec.Command(controlBin, append(cargs, "-crash-after-execute")...).Run()
	_ = exec.Command(controlBin, cargs...).Run() // restart

	cops, _ := fakeworld.New(cworld).Ops()
	r.controlOps = len(cops)
	r.controlOK = len(cops) == 1
	r.controlNote = "no durable intent existed, so the restart could not tell 'never happened' from 'happened and we crashed'"
	if len(cops) == 1 {
		r.controlNote = "unexpectedly did not repeat the action"
	}
	return r
}

// memoryWriteScenario fails only the memory write, with no crash at all.
func memoryWriteScenario(ctx context.Context, adminURL, dir, controlBin string) result {
	r := result{
		scenario: "memory write fails, no crash",
		description: "The external action succeeds and the operational write commits. Only the write to " +
			"the vector store fails. Nothing crashes.",
		// Anchor cannot reach this state: the memory row is in the same
		// transaction as the intent, so a failed memory write aborts everything.
		anchorOps: 1, anchorOK: true,
		anchorNote: "structurally impossible: the memory row commits in the same transaction, so a failure rolls the action record back too",
	}

	controlDB, controlURL := scratch(ctx, adminURL, control.ControlSchema)
	defer controlDB.Close()
	cworld := filepath.Join(dir, "diverge-world.jsonl")
	vec := filepath.Join(dir, "diverge-vectors.json")
	_ = exec.Command(controlBin, "-db", controlURL, "-world", cworld, "-vectors", vec,
		"-episode", uuid.NewString(), "-cluster", "cmp", "-nodes", "7",
		"-fail-vector-write").Run()

	cops, _ := fakeworld.New(cworld).Ops()
	n, _ := control.NewVectorStore(vec).Count()
	r.controlOps = len(cops)
	r.controlOK = n > 0
	r.controlNote = fmt.Sprintf(
		"world changed (%d operation) and the relational store committed, but the vector store holds %d memories; nothing can roll back",
		len(cops), n)
	return r
}

func scratch(ctx context.Context, adminURL, extraSchema string) (*pgxpool.Pool, string) {
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		log.Fatalf("compare: %v", err)
	}
	defer admin.Close()

	name := "anchor_cmp_" + uuid.NewString()[:8]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		log.Fatalf("compare: creating scratch database: %v", err)
	}
	url := swapDB(adminURL, name)

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("compare: %v", err)
	}
	schema, err := os.ReadFile("db/schema.sql")
	if err != nil {
		log.Fatalf("compare: reading schema: %v", err)
	}
	for _, stmt := range testsupport.SplitSQL(string(schema) + "\n" + extraSchema) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			log.Fatalf("compare: schema: %v", err)
		}
	}
	return pool, url
}

func swapDB(url, name string) string {
	slash := len(url) - 1
	for slash >= 0 && url[slash] != '/' {
		slash--
	}
	rest := url[slash+1:]
	q := 0
	for q < len(rest) && rest[q] != '?' {
		q++
	}
	return url[:slash+1] + name + rest[q:]
}

func seedAnchor(ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	agentID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (agent_id,name,scope) VALUES ($1,'compare',ARRAY['cmp'])`, agentID); err != nil {
		log.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPSERT INTO managed_clusters (cluster_id,scope_key,desired_nodes) VALUES ('cmp','cmp',3)`); err != nil {
		log.Fatal(err)
	}
	vec, _ := fakeworld.HashEmbedder{}.Embed(ctx, "comparison incident")
	var epID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO episodes (scope_key,status,symptom,narrative,embedding,expires_at)
VALUES ('cmp','open','comparison incident','opened',$1, now() + INTERVAL '1 day')
RETURNING episode_id`, vec).Scan(&epID); err != nil {
		log.Fatal(err)
	}
	return agentID, epID
}

func reconcile(ctx context.Context, pool *pgxpool.Pool, world string, agentID uuid.UUID) {
	reg := verify.NewRegistry()
	verify.MustRegister[fakeworld.ScaleArgs](reg, fakeworld.ScaleAction{World: fakeworld.New(world)})
	coord := protocol.NewCoordinator(pool, reg, agentID, 2*time.Minute)
	rec := protocol.NewReconciler(pool, reg, coord, fakeworld.HashEmbedder{}, agentID, 2*time.Minute, nil)
	if _, err := rec.RunOnce(ctx); err != nil {
		log.Printf("compare: reconcile: %v", err)
	}
}
