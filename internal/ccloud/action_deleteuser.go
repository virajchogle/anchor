package ccloud

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/virajchogle/anchor/internal/verify"
)

// ActionDeleteSQLUserType is the persisted action_type string.
const ActionDeleteSQLUserType = "delete_sql_user"

// DeleteSQLUserArgs are the arguments to the action.
type DeleteSQLUserArgs struct {
	ClusterID string `json:"cluster_id"`
	Username  string `json:"username"`
	Reason    string `json:"reason"`
}

// DeleteSQLUserAction revokes diagnostic access once an incident closes.
//
// This action exists to demonstrate the case create_sql_user does not: an
// operation whose effect is NOT attributable from world state alone.
//
// Creating a user leaves a thing behind whose name we chose, so finding it
// proves we made it. Deleting one leaves nothing. The user being absent is
// consistent with three different histories:
//
//   - we deleted it,
//   - an operator deleted it by hand,
//   - it never existed.
//
// State observation cannot separate those. Only the audit log can. So when the
// audit log is unavailable this verifier returns Unknown and escalates to a
// human, rather than reporting NotApplied and inviting a second delete, or
// reporting Applied and recording a deletion the agent may not have performed.
//
// This is the honest shape of exactly-once against a real API: it is a property
// of the protocol and the verifier together, and where evidence runs out the
// system stops and asks rather than guessing.
type DeleteSQLUserAction struct {
	Client   *Client
	Lookback time.Duration

	// RequireAuditProof, when true, refuses to accept absence as evidence even
	// as a fallback. It is the correct setting for any destructive action.
	RequireAuditProof bool
}

func (a DeleteSQLUserAction) Type() string { return ActionDeleteSQLUserType }

func (a DeleteSQLUserAction) Execute(ctx context.Context, args DeleteSQLUserArgs, idemKey string) (*verify.Receipt, error) {
	if args.Username == "" {
		return nil, fmt.Errorf("ccloud: delete_sql_user requires a username")
	}
	if err := a.Client.DeleteSQLUser(ctx, args.ClusterID, args.Username); err != nil {
		return nil, err
	}
	outcome, _ := json.Marshal(map[string]string{
		"username": args.Username, "cluster_id": args.ClusterID,
	})
	return &verify.Receipt{Outcome: outcome}, nil
}

// Verify refuses to claim attribution it does not have.
//
// This is the honest counterpart to create_sql_user, and the difference between
// them is the whole lesson.
//
// For a create, the username is DERIVED from the idempotency key, so an audit
// entry containing that name could only have come from this intent. Attribution
// is real.
//
// For a delete, the target is an existing name that this intent did not choose.
// An audit entry reading "user X was deleted" proves that a deletion happened.
// It does not prove that OURS did: an operator could have removed the same user
// through the console a second earlier, and the entry would look identical.
// Matching on it would be exactly the false-Applied this project exists to
// prevent, and an agent that records a deletion it did not perform has
// fabricated its own history.
//
// So there is only one conclusive verdict available here:
//
//	user still present  -> NotApplied. Whatever else happened, this did not.
//	user absent         -> Unknown. Absence is not attributable. Escalate.
//
// The lesson generalises: prefer actions that let you choose the identifier.
// Where the API does not allow it, exactly-once degrades to at-most-once plus a
// human, and saying so is better than pretending otherwise.
func (a DeleteSQLUserAction) Verify(ctx context.Context, args DeleteSQLUserArgs, idemKey, priorRef string) (verify.Verdict, error) {
	users, listErr := a.Client.ListSQLUsers(ctx, args.ClusterID)
	if listErr != nil {
		return verify.Verdict{
			Disposition: verify.Unknown,
			Reason: fmt.Sprintf(
				"the SQL user list for cluster %s could not be read (%v), so nothing is "+
					"known about whether this deletion ran",
				args.ClusterID, listErr),
		}, nil
	}

	for _, u := range users {
		if u.Name == args.Username {
			return verify.Verdict{
				Disposition: verify.NotApplied,
				Reason: fmt.Sprintf(
					"SQL user %q still exists on cluster %s, so this deletion did not take effect",
					args.Username, args.ClusterID),
			}, nil
		}
	}

	// The user is gone. Report what the audit log saw, as context for the human,
	// while being explicit that it is not proof of authorship.
	corroboration := "no matching audit entry was found"
	if entries, err := a.Client.AuditSince(ctx, time.Now().Add(-a.lookback()), 500); err == nil {
		for _, e := range entries {
			if e.Action != ActionDeleteSQLUser || e.ClusterID != args.ClusterID {
				continue
			}
			if name, perr := e.PayloadField("name"); perr == nil && name == args.Username {
				corroboration = fmt.Sprintf(
					"the audit log does record a deletion of this user (entry %s, via %s, actor %s), "+
						"but that entry carries no identifier tying it to this intent",
					e.ID, e.Source, actorOf(e))
				break
			}
		}
	}

	outcome, _ := json.Marshal(map[string]string{
		"username": args.Username, "cluster_id": args.ClusterID,
		"evidence": EvidenceNone,
	})
	return verify.Verdict{
		Disposition: verify.Unknown,
		Outcome:     outcome,
		Reason: fmt.Sprintf(
			"SQL user %q is absent from cluster %s, but absence is not attributable to this "+
				"intent: the target name was not derived from the idempotency key, so an "+
				"operator deleting the same user would look identical. %s. Escalating to a "+
				"human rather than recording a deletion this agent may not have performed.",
			args.Username, args.ClusterID, corroboration),
	}, nil
}

func (a DeleteSQLUserAction) lookback() time.Duration {
	if a.Lookback > 0 {
		return a.Lookback
	}
	return 24 * time.Hour
}

// Effect records that the cluster was touched. Access revocation changes no
// capacity, so DesiredNodes stays nil.
func (a DeleteSQLUserAction) Effect(args DeleteSQLUserArgs) *verify.ClusterEffect {
	return &verify.ClusterEffect{ClusterID: args.ClusterID, LastAction: a.Type()}
}

var _ verify.TypedAction[DeleteSQLUserArgs] = DeleteSQLUserAction{}
