package control_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/control"
	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/testsupport"
)

func setup(t *testing.T) (*pgxpool.Pool, string, string, string, string) {
	t.Helper()
	pool, dbURL := testsupport.NewDB(t)
	for _, stmt := range testsupport.SplitSQL(control.ControlSchema) {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("control schema: %v", err)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "controlagent")
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/controlagent").CombinedOutput()
	if err != nil {
		t.Fatalf("build controlagent: %v\n%s", err, out)
	}
	return pool, dbURL, bin, filepath.Join(dir, "world.jsonl"), filepath.Join(dir, "vectors.json")
}

// TestControl_DoubleActsOnCrash is the experiment the thesis rests on.
//
// The identical incident, the identical external API, and the identical crash
// point as TestChaos_CrashBetweenExecuteAndCommit. Anchor's reconciler settles
// that crash without repeating the action. This architecture cannot, because
// the only record of the action is written after the action, so the crash
// destroys the evidence that anything was attempted.
func TestControl_DoubleActsOnCrash(t *testing.T) {
	pool, dbURL, bin, worldPath, vecPath := setup(t)
	world := fakeworld.New(worldPath)
	episodeID := uuid.New()

	args := []string{
		"-db", dbURL, "-world", worldPath, "-vectors", vecPath,
		"-episode", episodeID.String(), "-cluster", "cluster-control", "-nodes", "5",
	}

	// First attempt: acts on the world, then dies before recording anything.
	if out, err := exec.Command(bin, append(args, "-crash-after-execute")...).CombinedOutput(); err == nil {
		t.Fatalf("expected the control agent to crash, it exited cleanly:\n%s", out)
	}

	ops, err := world.Ops()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("after the crash the world should show 1 operation, got %d", len(ops))
	}

	var runs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM control_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("expected no durable record of the action, got %d", runs)
	}
	t.Log("after crash: world changed once, database has no record it ever happened")

	// Restart. The agent looks for evidence, finds none, and acts again.
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("restart failed: %v\n%s", err, out)
	}
	t.Logf("restart output: %s", out)

	ops, err = world.Ops()
	if err != nil {
		t.Fatal(err)
	}

	// THE RESULT.
	if len(ops) != 2 {
		t.Fatalf("expected the control architecture to double-act (2 operations), got %d.\n"+
			"If this ever reports 1, the control is no longer modelling the architecture "+
			"it is meant to represent and the comparison is invalid.", len(ops))
	}
	t.Logf("CONTROL DOUBLE-ACTED: the external world was scaled %d times for one incident", len(ops))
}

// TestControl_MemoryDivergesFromReality is the second, quieter failure.
//
// Even with no crash at all, the memory write is a second transaction against a
// second store. When it fails, the relational write has already committed. The
// world was changed, the operational database says so, and the agent's semantic
// memory has no idea. Nothing rolls back because nothing can.
//
// This is the failure that a single serializable transaction in CockroachDB
// makes structurally impossible, and it is the direct answer to "why not
// Postgres plus Pinecone".
func TestControl_MemoryDivergesFromReality(t *testing.T) {
	pool, dbURL, bin, worldPath, vecPath := setup(t)
	world := fakeworld.New(worldPath)
	vectors := control.NewVectorStore(vecPath)
	episodeID := uuid.New()

	out, err := exec.Command(bin,
		"-db", dbURL, "-world", worldPath, "-vectors", vecPath,
		"-episode", episodeID.String(), "-cluster", "cluster-diverge", "-nodes", "7",
		"-fail-vector-write",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("control agent failed unexpectedly: %v\n%s", err, out)
	}
	t.Logf("agent output: %s", out)

	// The world changed.
	ops, _ := world.Ops()
	if len(ops) != 1 {
		t.Fatalf("expected exactly 1 external operation, got %d", len(ops))
	}

	// The operational database committed.
	var version int
	if err := pool.QueryRow(context.Background(),
		`SELECT version FROM control_clusters WHERE cluster_id='cluster-diverge'`).Scan(&version); err != nil {
		t.Fatalf("expected committed cluster state: %v", err)
	}
	if version == 0 {
		t.Fatal("expected the cluster version to have advanced")
	}

	// And memory knows nothing about it.
	n, err := vectors.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected the vector store to be empty after a failed write, got %d", n)
	}

	t.Log("DIVERGENCE: the world was changed and the relational store committed, " +
		"but the agent's semantic memory contains no record of it")
}

// TestControl_SucceedsWhenNothingFails guards against a strawman. The control
// must be a working implementation under normal conditions, or the comparison
// proves nothing.
func TestControl_SucceedsWhenNothingFails(t *testing.T) {
	_, dbURL, bin, worldPath, vecPath := setup(t)
	world := fakeworld.New(worldPath)
	vectors := control.NewVectorStore(vecPath)
	episodeID := uuid.New()

	args := []string{
		"-db", dbURL, "-world", worldPath, "-vectors", vecPath,
		"-episode", episodeID.String(), "-cluster", "cluster-happy", "-nodes", "5",
	}
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("control agent failed on the happy path: %v\n%s", err, out)
	}

	ops, _ := world.Ops()
	if len(ops) != 1 {
		t.Errorf("expected 1 operation, got %d", len(ops))
	}
	if n, _ := vectors.Count(); n != 1 {
		t.Errorf("expected 1 memory record, got %d", n)
	}

	// And it correctly refuses to repeat itself when it does have a record.
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("second run failed: %v\n%s", err, out)
	}
	ops, _ = world.Ops()
	if len(ops) != 1 {
		t.Errorf("control should not repeat an action it has a record of, got %d operations", len(ops))
	}
	t.Log("control behaves correctly when nothing fails; the failure is structural, not sloppiness")
}
