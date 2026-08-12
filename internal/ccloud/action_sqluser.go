package ccloud

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/virajchogle/anchor/internal/verify"
)

// ActionCreateSQLUserType is the persisted action_type string.
const ActionCreateSQLUserType = "create_sql_user"

// CreateSQLUserArgs are the arguments to the action.
//
// Note what is absent: the username. It is not an argument because it is derived
// from the idempotency key, which is itself derived from these arguments. Making
// the caller supply a name would let two logically identical requests produce
// different names and defeat deduplication entirely.
type CreateSQLUserArgs struct {
	ClusterID string `json:"cluster_id"`
	// Purpose is recorded in memory so a future operator can tell why the agent
	// provisioned this user.
	Purpose string `json:"purpose"`
}

// CreateSQLUserAction provisions a scoped diagnostic SQL user during an incident.
//
// This is the reference action for Anchor's exactly-once protocol because it is
// genuinely non-idempotent: creating the same user twice is an error, not a
// no-op. Recovering from a crash by blindly retrying would fail loudly at best
// and, on an API with different semantics, act twice at worst.
//
// It is also the action for which honest verification is achievable, which is
// the real reason it was chosen. See Verify.
type CreateSQLUserAction struct {
	Client *Client

	// Lookback bounds how far back the audit log is searched. Zero means 24h.
	// The derived username embeds the idempotency key and is therefore globally
	// unique, so a generous window costs only scan time, never correctness.
	Lookback time.Duration

	// AuditLimit caps entries fetched per verification. Zero means 500.
	AuditLimit int
}

func (a CreateSQLUserAction) Type() string { return ActionCreateSQLUserType }

// UsernameFor derives the SQL username from the idempotency key.
//
// This derivation is the linchpin of attribution. Because the name provably
// encodes this specific intent, finding a user by that name anywhere in the
// cluster is proof that THIS action ran, not merely that the world happens to
// look the way we wanted. It converts an unattributable state observation into
// attributable evidence.
//
// 16 hex characters of a SHA-256 gives 64 bits, which is far beyond collision
// risk for the number of actions an on-call agent will ever take, and keeps the
// identifier within SQL identifier length limits.
func UsernameFor(idemKey string) string {
	n := 16
	if len(idemKey) < n {
		n = len(idemKey)
	}
	return "anchor_" + strings.ToLower(idemKey[:n])
}

// Execute creates the user. It runs outside any transaction and may leave the
// user created even when it returns an error, which is the case Verify exists
// to settle.
func (a CreateSQLUserAction) Execute(ctx context.Context, args CreateSQLUserArgs, idemKey string) (*verify.Receipt, error) {
	username := UsernameFor(idemKey)

	password, err := randomPassword()
	if err != nil {
		return nil, fmt.Errorf("ccloud: generating password: %w", err)
	}

	if err := a.Client.CreateSQLUser(ctx, args.ClusterID, username, password); err != nil {
		return nil, err
	}

	// The password is deliberately not returned and not logged. It exists only
	// so the account is not created without one; the operator rotates it through
	// the console, or a later revision stores it in Secrets Manager. Putting a
	// credential into the outcome column would write it into memory rows that
	// the recall path reads and the observability panel renders.
	outcome, _ := json.Marshal(map[string]string{
		"username":   username,
		"cluster_id": args.ClusterID,
	})

	// external_ref is filled by looking the operation up in the audit log rather
	// than invented here, so a committed intent always points at evidence that
	// exists outside this process.
	ref := ""
	if entry, err := a.findAuditEntry(ctx, args.ClusterID, username); err == nil && entry != nil {
		ref = entry.ID
	}

	return &verify.Receipt{ExternalRef: ref, Outcome: outcome}, nil
}

// Verify establishes whether this specific action reached the Cloud control
// plane, using two independent sources of attributable evidence.
//
// Neither source is "does the cluster look right". Both turn on the derived
// username, which encodes the idempotency key:
//
//  1. The organization audit log. The strongest evidence, because it records
//     what the control plane did, carries a unique operation id for external_ref,
//     and distinguishes a CLI action by the agent from a human in the console.
//  2. The cluster's SQL user list. Weaker on timing and provenance, but the name
//     is still ours, so its presence is attributable.
//
// The verdict is Unknown, never NotApplied, whenever the evidence could be
// incomplete. Concluding "it did not happen" from a failed read is how an agent
// ends up doing something twice.
func (a CreateSQLUserAction) Verify(ctx context.Context, args CreateSQLUserArgs, idemKey, priorRef string) (verify.Verdict, error) {
	username := UsernameFor(idemKey)

	entry, auditErr := a.findAuditEntry(ctx, args.ClusterID, username)
	if auditErr == nil && entry != nil {
		outcome, _ := json.Marshal(map[string]string{
			"username":   username,
			"cluster_id": args.ClusterID,
			"audit_id":   entry.ID,
			"source":     entry.Source,
			"actor":      actorOf(*entry),
			"evidence":   EvidenceAuditLog,
		})
		return verify.Verdict{
			Disposition: verify.Applied,
			ExternalRef: entry.ID,
			Outcome:     outcome,
			Reason: fmt.Sprintf(
				"audit log entry %s records %s for user %q on cluster %s at %s via %s",
				entry.ID, entry.Action, username, args.ClusterID,
				entry.CreatedAt.UTC().Format(time.RFC3339), entry.Source),
		}, nil
	}

	// Audit log unreadable. Fall back to the user list, which is still
	// attributable because we chose the name.
	users, listErr := a.Client.ListSQLUsers(ctx, args.ClusterID)
	if listErr != nil {
		return verify.Verdict{
			Disposition: verify.Unknown,
			Reason: fmt.Sprintf(
				"could not establish ground truth: audit log read failed (%v) and SQL user list failed (%v)",
				auditErr, listErr),
		}, nil
	}

	for _, u := range users {
		if u.Name == username {
			outcome, _ := json.Marshal(map[string]string{
				"username":   username,
				"cluster_id": args.ClusterID,
				"evidence":   EvidenceUserList,
			})
			// The username itself is the external reference here. It is not
			// invented: it names a durable object that exists in the external
			// system and encodes this intent, so the committed row still points
			// at evidence outside this process.
			ref := priorRef
			if ref == "" {
				ref = "sqluser:" + username
			}
			return verify.Verdict{
				Disposition: verify.Applied,
				ExternalRef: ref,
				Outcome:     outcome,
				Reason: fmt.Sprintf(
					"SQL user %q exists on cluster %s and its name encodes this intent's "+
						"idempotency key (%s)",
					username, args.ClusterID, auditNote(auditErr)),
			}, nil
		}
	}

	// The user is absent. That is only conclusive if the audit read failed for a
	// benign reason rather than leaving us blind to a window we did not see.
	if auditErr != nil && !isTruncatedWindow(auditErr) {
		return verify.Verdict{
			Disposition: verify.Unknown,
			Reason: fmt.Sprintf(
				"SQL user %q is absent, but the audit log could not be read (%v), "+
					"so absence is not proof the action did not run",
				username, auditErr),
		}, nil
	}

	return verify.Verdict{
		Disposition: verify.NotApplied,
		Reason: fmt.Sprintf(
			"no audit entry and no SQL user named %q on cluster %s; the action did not reach the control plane",
			username, args.ClusterID),
	}, nil
}

// Effect records that the cluster was touched without asserting a node count.
func (a CreateSQLUserAction) Effect(args CreateSQLUserArgs) *verify.ClusterEffect {
	return &verify.ClusterEffect{
		ClusterID:  args.ClusterID,
		LastAction: a.Type(),
		// DesiredNodes stays nil: provisioning a user changes no capacity.
	}
}

// findAuditEntry searches the audit log for the creation of a specific username.
func (a CreateSQLUserAction) findAuditEntry(ctx context.Context, clusterID, username string) (*AuditEntry, error) {
	lookback := a.Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	limit := a.AuditLimit
	if limit <= 0 {
		limit = 500
	}

	since := time.Now().Add(-lookback)
	entries, err := a.Client.AuditSince(ctx, since, limit)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if e.Action != ActionCreateSQLUser || e.ClusterID != clusterID {
			continue
		}
		name, perr := e.PayloadField("name")
		if perr != nil {
			// A payload we cannot decode is not evidence of absence.
			return nil, perr
		}
		if name == username {
			found := e
			return &found, nil
		}
	}

	// Hitting the limit exactly means the window may have been truncated and the
	// entry could be just beyond it. Reporting "not found" here would be a guess
	// dressed up as a fact.
	if len(entries) >= limit {
		return nil, errTruncatedWindow{limit: limit, since: since}
	}
	return nil, nil
}

type errTruncatedWindow struct {
	limit int
	since time.Time
}

func (e errTruncatedWindow) Error() string {
	return fmt.Sprintf("audit log returned the full limit of %d entries since %s; "+
		"the window may be truncated and absence cannot be concluded",
		e.limit, e.since.UTC().Format(time.RFC3339))
}

func isTruncatedWindow(err error) bool {
	_, ok := err.(errTruncatedWindow)
	return ok
}

// Evidence source labels, recorded in the outcome so the observability panel
// can show which source settled a verdict rather than just its conclusion.
const (
	EvidenceAuditLog = "audit_log"
	EvidenceUserList = "sql_user_list"
)

// auditNote describes the audit log's contribution without misreporting a
// successful-but-empty read as a failure. Formatting a nil error produced
// "audit log was unavailable (<nil>)", which claimed a failure that never
// happened and would have misled an operator reading the verdict.
func auditNote(err error) string {
	switch {
	case err == nil:
		return "the audit log had no matching entry yet, which is expected while it catches up"
	case isTruncatedWindow(err):
		return "the audit log window was truncated"
	default:
		return fmt.Sprintf("the audit log could not be read: %v", err)
	}
}

func actorOf(e AuditEntry) string {
	if e.ServiceAcct != "" {
		return e.ServiceAcct
	}
	return e.UserEmail
}

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Strip characters that need escaping in a connection string.
	s := base64.RawURLEncoding.EncodeToString(b)
	return strings.NewReplacer("-", "x", "_", "y").Replace(s), nil
}

// Compile-time proof that the action satisfies the full contract. Without
// Verify and Effect this line does not build, which is the enforcement the
// design depends on.
var _ verify.TypedAction[CreateSQLUserArgs] = CreateSQLUserAction{}
