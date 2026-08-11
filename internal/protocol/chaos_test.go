package protocol_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/protocol"
	"github.com/virajchogle/anchor/internal/verify"
)

func run(t *testing.T, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// TestChaos_CrashBetweenExecuteAndCommit is the central test of this project.
//
// A real process performs the external action and is then killed before it can
// record that it did. On restart the reconciler must discover the orphaned
// intent, establish from the external system that the action DID happen, commit
// memory that matches reality, and above all not perform the action a second
// time.
func TestChaos_CrashBetweenExecuteAndCommit(t *testing.T) {
	ctx := context.Background()
	pool, dbURL := newTestDB(t)
	agentID, episodeID := seed(t, pool, "cluster-chaos", 3)

	world := fakeworld.New(worldPath(t))
	bin := buildChaosAgent(t)

	// --- The crash -------------------------------------------------------
	// A short lease so the reconciler may legitimately claim it moments later,
	// rather than the test reaching into the database to fake an expiry.
	cmd := exec.Command(bin,
		"-db", dbURL,
		"-world", world.Path(),
		"-episode", episodeID.String(),
		"-agent", agentID.String(),
		"-cluster", "cluster-chaos",
		"-nodes", "5",
		"-lease", "2s",
		"-crash-after-execute",
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected the agent to die, but it exited cleanly.\noutput: %s", out)
	}
	if code := exitErr.ExitCode(); code != 9 {
		t.Fatalf("expected hard exit code 9 from phase 2, got %d\noutput: %s", code, out)
	}
	t.Logf("agent killed as intended:\n%s", out)

	// --- State immediately after the crash -------------------------------
	ops, err := world.Ops()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected the external world to have been acted on exactly once, got %d ops", len(ops))
	}
	orphanToken := ops[0].IdemToken

	intent := loadIntent(t, pool, orphanToken)
	if intent.State != protocol.StatePending {
		t.Fatalf("crashed intent should still be PENDING, got %s", intent.State)
	}
	if intent.ExternalRef != "" {
		t.Fatalf("crashed process should not have recorded an external_ref, got %q", intent.ExternalRef)
	}
	// This is the divergence the whole project targets: the world changed, and
	// the agent's memory does not know it.
	assertEpisodeStatus(t, pool, episodeID, "open")
	assertCluster(t, pool, "cluster-chaos", 3, 0)

	// --- Restart and reconcile -------------------------------------------
	time.Sleep(2200 * time.Millisecond) // let the lease lapse naturally

	reg := verify.NewRegistry()
	verify.MustRegister[fakeworld.ScaleArgs](reg, fakeworld.ScaleAction{World: world})

	reconcilerID := uuid.New()
	mustInsertAgent(t, pool, reconcilerID)

	coord := protocol.NewCoordinator(pool, reg, reconcilerID, 2*time.Minute)
	rec := protocol.NewReconciler(pool, reg, coord, fakeworld.HashEmbedder{},
		reconcilerID, 2*time.Minute, nil)

	stats, err := rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("reconciliation pass: %v", err)
	}
	if stats.Claimed != 1 || stats.Committed != 1 {
		t.Fatalf("expected to claim and commit exactly 1 orphaned intent, got %+v", stats)
	}

	// --- The assertions that matter --------------------------------------
	recovered := loadIntent(t, pool, orphanToken)
	if recovered.State != protocol.StateCommitted {
		t.Errorf("intent should be COMMITTED after reconciliation, got %s", recovered.State)
	}
	if recovered.ExternalRef == "" {
		t.Error("reconciler committed without recording the external reference it verified against")
	}

	// THE assertion. The action must not have happened twice.
	ops, err = world.Ops()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("the reconciler acted again: external world has %d operations, want exactly 1", len(ops))
	}

	// Memory now matches reality, and is pinned against TTL decay because an
	// action committed against it.
	assertEpisodeStatus(t, pool, episodeID, "resolved")
	assertEpisodePinned(t, pool, episodeID)
	assertCluster(t, pool, "cluster-chaos", 5, 1)

	// --- Reconciling again must be a no-op --------------------------------
	stats, err = rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second reconciliation pass: %v", err)
	}
	if stats.Claimed != 0 {
		t.Errorf("a resolved intent was claimed again: %+v", stats)
	}
	ops, _ = world.Ops()
	if len(ops) != 1 {
		t.Fatalf("second reconciliation pass acted on the world: %d operations", len(ops))
	}
}

// TestChaos_RerunAfterRecoveryDoesNotReact covers the other half of recovery:
// the original agent comes back and retries the same logical action. It must be
// told the action already happened rather than performing it again.
func TestChaos_RerunAfterRecoveryDoesNotReact(t *testing.T) {
	ctx := context.Background()
	pool, dbURL := newTestDB(t)
	agentID, episodeID := seed(t, pool, "cluster-rerun", 3)

	world := fakeworld.New(worldPath(t))
	bin := buildChaosAgent(t)

	args := []string{
		"-db", dbURL, "-world", world.Path(),
		"-episode", episodeID.String(), "-agent", agentID.String(),
		"-cluster", "cluster-rerun", "-nodes", "5", "-lease", "2s",
	}

	// Crash mid-action.
	cmd := exec.Command(bin, append(args, "-crash-after-execute")...)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected a crash, agent exited cleanly:\n%s", out)
	}

	time.Sleep(2200 * time.Millisecond)

	// Recover.
	reg := verify.NewRegistry()
	verify.MustRegister[fakeworld.ScaleArgs](reg, fakeworld.ScaleAction{World: world})
	recID := uuid.New()
	mustInsertAgent(t, pool, recID)
	coord := protocol.NewCoordinator(pool, reg, recID, 2*time.Minute)
	rec := protocol.NewReconciler(pool, reg, coord, fakeworld.HashEmbedder{}, recID, 2*time.Minute, nil)
	if _, err := rec.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The original agent retries the identical action.
	out := run(t, 60*time.Second, bin, args...)
	if !strings.Contains(out, "ALREADY_COMMITTED") {
		t.Errorf("retry should have been told the action already committed, got:\n%s", out)
	}

	ops, _ := world.Ops()
	if len(ops) != 1 {
		t.Fatalf("retry after recovery double-acted: %d operations, want 1", len(ops))
	}
}
