package ccloud_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/virajchogle/anchor/internal/ccloud"
	"github.com/virajchogle/anchor/internal/protocol"
	"github.com/virajchogle/anchor/internal/verify"
)

// These tests mutate a real CockroachDB Cloud organization: they create and
// delete SQL users. They are opt-in so a plain `go test ./...` never touches
// live infrastructure.
//
//	ANCHOR_CCLOUD_LIVE=1 CCLOUD_CLUSTER_ID=<id> go test ./internal/ccloud/ -v

// evidenceOf reports which source settled a verdict.
func evidenceOf(t *testing.T, v verify.Verdict) string {
	t.Helper()
	if len(v.Outcome) == 0 {
		return ""
	}
	var fields map[string]string
	if err := json.Unmarshal(v.Outcome, &fields); err != nil {
		t.Fatalf("decoding verdict outcome: %v", err)
	}
	return fields["evidence"]
}

func liveClient(t *testing.T) (*ccloud.Client, string) {
	t.Helper()
	if os.Getenv("ANCHOR_CCLOUD_LIVE") != "1" {
		t.Skip("set ANCHOR_CCLOUD_LIVE=1 to run tests that mutate a real cluster")
	}
	clusterID := os.Getenv("CCLOUD_CLUSTER_ID")
	if clusterID == "" {
		t.Skip("CCLOUD_CLUSTER_ID is not set")
	}
	c := ccloud.New()
	if err := c.Whoami(context.Background()); err != nil {
		t.Skipf("ccloud is not authenticated: %v", err)
	}
	return c, clusterID
}

// TestLive_CreateSQLUser_VerifiesViaAuditLog is the end-to-end proof of gate 3:
// that an action taken through ccloud leaves a trace attributable to this
// specific intent, rather than merely leaving the world in the desired shape.
func TestLive_CreateSQLUser_VerifiesViaAuditLog(t *testing.T) {
	ctx := context.Background()
	client, clusterID := liveClient(t)

	action := ccloud.CreateSQLUserAction{Client: client, Lookback: 2 * time.Hour}
	args := ccloud.CreateSQLUserArgs{ClusterID: clusterID, Purpose: "integration test"}

	// A real idempotency key, derived exactly as the protocol derives it.
	episodeID := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	idemKey, err := protocol.IdemKey(episodeID, action.Type(), args)
	if err != nil {
		t.Fatal(err)
	}
	username := ccloud.UsernameFor(idemKey)
	t.Logf("derived username: %s", username)

	t.Cleanup(func() {
		if err := client.DeleteSQLUser(context.Background(), clusterID, username); err != nil {
			t.Logf("cleanup: could not delete %s: %v", username, err)
		}
	})

	// --- Before acting, verification must say NotApplied ------------------
	before, err := action.Verify(ctx, args, idemKey, "")
	if err != nil {
		t.Fatalf("pre-verify: %v", err)
	}
	if before.Disposition != verify.NotApplied {
		t.Fatalf("before executing, verdict should be NOT_APPLIED, got %s: %s",
			before.Disposition, before.Reason)
	}
	t.Logf("pre-verify: %s (%s)", before.Disposition, before.Reason)

	// --- Execute ----------------------------------------------------------
	receipt, err := action.Execute(ctx, args, idemKey)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	t.Logf("executed, external_ref=%q", receipt.ExternalRef)

	// --- Verification must now say Applied, with evidence -----------------
	// Poll until the AUDIT LOG specifically settles it, not just until the
	// verdict flips to Applied. The SQL user list would satisfy Applied almost
	// immediately, and accepting that would leave the audit path, which is the
	// strongest evidence and the basis of the whole attribution claim, untested.
	var after verify.Verdict
	deadline := time.Now().Add(120 * time.Second)
	for {
		after, err = action.Verify(ctx, args, idemKey, receipt.ExternalRef)
		if err != nil {
			t.Fatalf("post-verify: %v", err)
		}
		if evidenceOf(t, after) == ccloud.EvidenceAuditLog || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if got := evidenceOf(t, after); got != ccloud.EvidenceAuditLog {
		t.Errorf("verdict was settled by %q, not the audit log, within the deadline; "+
			"the attributable-evidence path is what gate 3 rests on", got)
	}

	if after.Disposition != verify.Applied {
		t.Fatalf("after executing, verdict should be APPLIED, got %s: %s",
			after.Disposition, after.Reason)
	}
	t.Logf("post-verify: %s", after.Reason)

	if after.ExternalRef == "" {
		t.Error("Applied verdict carries no external reference")
	}
	// The reason must cite real evidence, not restate the request.
	if !strings.Contains(after.Reason, username) {
		t.Errorf("verdict reason does not name the derived user: %s", after.Reason)
	}
}

// TestLive_VerifyIsAttributable is the assertion the whole design turns on.
//
// A second, different intent against the same cluster must verify as NotApplied
// even though the cluster now contains a user created by the first intent. A
// verifier that merely observed "a user exists" would wrongly report Applied and
// the agent would skip an action it never took.
func TestLive_VerifyIsAttributable(t *testing.T) {
	ctx := context.Background()
	client, clusterID := liveClient(t)

	action := ccloud.CreateSQLUserAction{Client: client, Lookback: 2 * time.Hour}
	args := ccloud.CreateSQLUserArgs{ClusterID: clusterID, Purpose: "attribution test"}

	firstKey, _ := protocol.IdemKey("11111111-1111-1111-1111-111111111111", action.Type(), args)
	secondKey, _ := protocol.IdemKey("22222222-2222-2222-2222-222222222222", action.Type(), args)

	if firstKey == secondKey {
		t.Fatal("different episodes must derive different keys")
	}

	firstUser := ccloud.UsernameFor(firstKey)
	t.Cleanup(func() {
		_ = client.DeleteSQLUser(context.Background(), clusterID, firstUser)
	})

	if _, err := action.Execute(ctx, args, firstKey); err != nil {
		t.Fatalf("executing the first action: %v", err)
	}

	// The second intent never ran. The cluster is nonetheless in a state where a
	// naive state-observing verifier would see "a diagnostic user exists".
	verdict, err := action.Verify(ctx, args, secondKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Disposition == verify.Applied {
		t.Fatalf("verifier attributed another intent's action to this one; "+
			"this is the false-Applied that would make the agent skip a real action.\n%s",
			verdict.Reason)
	}
	t.Logf("correctly refused to attribute: %s (%s)", verdict.Disposition, verdict.Reason)
}
