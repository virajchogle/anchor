package payments_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/fakeworld"
	"github.com/virajchogle/anchor/internal/payments"
	"github.com/virajchogle/anchor/internal/protocol"
	"github.com/virajchogle/anchor/internal/testsupport"
	"github.com/virajchogle/anchor/internal/verify"
)

const charge = "ch_9f2a"

func setup(t *testing.T, honorKeys bool) (*pgxpool.Pool, *payments.Ledger, *verify.Registry, *protocol.Coordinator, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pool, _ := testsupport.NewDB(t)

	ledger := payments.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), honorKeys)
	reg := verify.NewRegistry()
	verify.MustRegister[payments.RefundArgs](reg, payments.RefundAction{Ledger: ledger})

	agentID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO agents (agent_id,name,scope) VALUES ($1,'billing',ARRAY['billing'])`,
		agentID); err != nil {
		t.Fatal(err)
	}
	vec, _ := fakeworld.HashEmbedder{}.Embed(ctx, "customer disputed a duplicate charge")
	var epID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO episodes (scope_key,status,symptom,narrative,embedding,expires_at)
VALUES ('billing','open','customer disputed a duplicate charge','opened',$1, now() + INTERVAL '1 day')
RETURNING episode_id`, vec).Scan(&epID); err != nil {
		t.Fatal(err)
	}
	return pool, ledger, reg, protocol.NewCoordinator(pool, reg, agentID, 2*time.Minute), agentID, epID
}

// TestPayments_ProtocolIsDomainAgnostic is the point of this package. A refund
// has nothing to do with databases, yet the coordinator, the idempotency key
// derivation and the verdict contract are used unchanged.
func TestPayments_ProtocolIsDomainAgnostic(t *testing.T) {
	ctx := context.Background()
	pool, ledger, reg, coord, _, epID := setup(t, true)

	args := payments.RefundArgs{ChargeID: charge, AmountCents: 4200, Reason: "duplicate charge"}
	intent, disp, err := coord.Intend(ctx, epID, payments.ActionRefundType, args)
	if err != nil || disp != protocol.Owned {
		t.Fatalf("phase 1: %v (%s)", err, disp)
	}

	receipt, err := reg.Execute(ctx, payments.ActionRefundType, intent.Args, intent.IdemKey)
	if err != nil {
		t.Fatal(err)
	}

	verdict, err := reg.Verify(ctx, payments.ActionRefundType, intent.Args, intent.IdemKey, receipt.ExternalRef)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Disposition != verify.Applied {
		t.Fatalf("expected APPLIED, got %s: %s", verdict.Disposition, verdict.Reason)
	}

	vec, _ := fakeworld.HashEmbedder{}.Embed(ctx, "refunded")
	if err := coord.CommitAtomic(ctx, protocol.Commit{
		IdemKey: intent.IdemKey,
		Receipt: &verify.Receipt{ExternalRef: verdict.ExternalRef, Outcome: verdict.Outcome},
		Cluster: nil, // a refund changes no tracked resource, and that is fine
		Memory: protocol.MemoryWrite{EpisodeID: epID, Narrative: "refunded the duplicate charge",
			Outcome: "resolved", Embedding: vec},
	}); err != nil {
		t.Fatalf("phase 3: %v", err)
	}

	cents, count, _ := ledger.TotalRefunded(charge)
	if count != 1 || cents != 4200 {
		t.Fatalf("customer should see exactly one refund of 4200, saw %d refund(s) totalling %d", count, cents)
	}

	var state string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM action_intents WHERE idem_key=$1`, intent.IdemKey).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != protocol.StateCommitted {
		t.Errorf("intent state = %s", state)
	}
}

// TestPayments_CrashDoesNotRefundTwice is the same crash as the infrastructure
// chaos test, with money at stake instead of capacity.
func TestPayments_CrashDoesNotRefundTwice(t *testing.T) {
	ctx := context.Background()
	pool, ledger, reg, coord, agentID, epID := setup(t, true)

	args := payments.RefundArgs{ChargeID: charge, AmountCents: 4200, Reason: "duplicate charge"}
	intent, _, err := coord.Intend(ctx, epID, payments.ActionRefundType, args)
	if err != nil {
		t.Fatal(err)
	}

	// Phase 2 happens. Phase 3 never does: the process dies here.
	if _, err := reg.Execute(ctx, payments.ActionRefundType, intent.Args, intent.IdemKey); err != nil {
		t.Fatal(err)
	}

	cents, count, _ := ledger.TotalRefunded(charge)
	if count != 1 {
		t.Fatalf("expected the money to have moved once, got %d", count)
	}
	t.Logf("after the crash: customer refunded %d cents, database has no record of it", cents)

	// Expire the lease and let the reconciler settle it.
	if _, err := pool.Exec(ctx,
		`UPDATE action_intents SET lease_expires = now() - INTERVAL '1 second' WHERE state='PENDING'`); err != nil {
		t.Fatal(err)
	}
	rec := protocol.NewReconciler(pool, reg, coord, fakeworld.HashEmbedder{}, agentID, 2*time.Minute, nil)
	stats, err := rec.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Committed != 1 {
		t.Fatalf("expected the reconciler to commit 1 intent, got %+v", stats)
	}

	// THE assertion: the customer was not paid twice.
	cents, count, _ = ledger.TotalRefunded(charge)
	if count != 1 || cents != 4200 {
		t.Fatalf("customer was refunded %d times totalling %d cents; recovery must not re-pay", count, cents)
	}
	t.Log("recovered without issuing a second refund")
}

// TestPayments_UnattributableProviderEscalates covers the realistic case of a
// provider that does not echo client idempotency keys. The refund exists, and
// nothing proves this intent made it.
func TestPayments_UnattributableProviderEscalates(t *testing.T) {
	ctx := context.Background()
	_, ledger, reg, coord, _, epID := setup(t, false)

	args := payments.RefundArgs{ChargeID: charge, AmountCents: 4200, Reason: "duplicate charge"}
	intent, _, err := coord.Intend(ctx, epID, payments.ActionRefundType, args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Execute(ctx, payments.ActionRefundType, intent.Args, intent.IdemKey); err != nil {
		t.Fatal(err)
	}

	verdict, err := reg.Verify(ctx, payments.ActionRefundType, intent.Args, intent.IdemKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Disposition != verify.Unknown {
		t.Fatalf("a provider that does not record client keys cannot support attribution; "+
			"expected UNKNOWN, got %s: %s", verdict.Disposition, verdict.Reason)
	}
	if _, count, _ := ledger.TotalRefunded(charge); count != 1 {
		t.Errorf("expected exactly one refund on the ledger, got %d", count)
	}
	t.Logf("correctly refused to attribute: %s", verdict.Reason)
}
