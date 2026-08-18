// Package payments is Anchor's second domain, and it exists to answer one
// question: does the protocol generalise, or is it a CockroachDB trick?
//
// Nothing here imports the infrastructure code. It implements the same
// verify.TypedAction interface against a completely different external system,
// and the coordinator, the reconciler, the idempotency key derivation and the
// three-valued verdict all work unchanged.
//
// Refunds are the honest example because the cost of getting it wrong is
// legible. At-least-once against a scale operation wastes money on capacity.
// At-least-once against a refund pays a customer twice, and no amount of
// retrying makes that safe.
package payments

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/virajchogle/anchor/internal/verify"
)

// Refund is one entry in the provider's ledger.
type Refund struct {
	ID          string    `json:"id"`
	ChargeID    string    `json:"charge_id"`
	AmountCents int       `json:"amount_cents"`
	At          time.Time `json:"at"`

	// IdemToken is the client-supplied idempotency key, recorded only when the
	// provider honours them. Stripe and its peers do; plenty of older payment
	// APIs and most internal ledgers do not, and that difference decides whether
	// verification can attribute a refund to a particular call.
	IdemToken string `json:"idem_token,omitempty"`
}

// Ledger is an append-only refund log standing in for a payment provider.
//
// Appending rather than upserting is the whole point: issuing the same refund
// twice produces two entries and the customer is paid twice. A store that
// deduplicated would hide the failure this package exists to expose.
type Ledger struct {
	mu   sync.Mutex
	path string

	// HonorIdempotencyKeys models a provider that records the client's token.
	// With it false, the ledger is a provider that does not, and verification
	// degrades from proof to inference. Both are real; the code should be honest
	// about which one it is talking to.
	HonorIdempotencyKeys bool
}

func NewLedger(path string, honorKeys bool) *Ledger {
	return &Ledger{path: path, HonorIdempotencyKeys: honorKeys}
}

func (l *Ledger) Path() string { return l.path }

// Issue appends a refund. It never deduplicates.
func (l *Ledger) Issue(r Refund) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.HonorIdempotencyKeys {
		r.IdemToken = ""
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	// The process may be killed immediately after this returns. A refund lost to
	// a buffer would make the ledger look untouched when money had already moved.
	return f.Sync()
}

func (l *Ledger) All() ([]Refund, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Refund
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r Refund
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("payments: corrupt ledger line: %w", err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// TotalRefunded is what a customer would actually see on their statement.
func (l *Ledger) TotalRefunded(chargeID string) (int, int, error) {
	all, err := l.All()
	if err != nil {
		return 0, 0, err
	}
	cents, count := 0, 0
	for _, r := range all {
		if r.ChargeID == chargeID {
			cents += r.AmountCents
			count++
		}
	}
	return cents, count, nil
}

// RefundArgs are the arguments to the refund action.
type RefundArgs struct {
	ChargeID    string `json:"charge_id"`
	AmountCents int    `json:"amount_cents"`
	Reason      string `json:"reason"`
}

// RefundAction issues a refund through the provider.
type RefundAction struct {
	Ledger *Ledger
}

const ActionRefundType = "issue_refund"

func (a RefundAction) Type() string { return ActionRefundType }

func (a RefundAction) Execute(ctx context.Context, args RefundArgs, idemKey string) (*verify.Receipt, error) {
	if args.AmountCents <= 0 {
		return nil, fmt.Errorf("payments: refund amount must be positive")
	}
	ref := "re_" + idemKey[:16]
	if err := a.Ledger.Issue(Refund{
		ID: ref, ChargeID: args.ChargeID, AmountCents: args.AmountCents,
		At: time.Now().UTC(), IdemToken: idemKey,
	}); err != nil {
		return nil, err
	}
	outcome, _ := json.Marshal(map[string]any{
		"refund_id": ref, "charge_id": args.ChargeID, "amount_cents": args.AmountCents,
	})
	return &verify.Receipt{ExternalRef: ref, Outcome: outcome}, nil
}

// Verify asks the provider whether this refund exists, and is careful about
// what the answer proves.
//
// When the provider honours idempotency keys, a ledger entry carrying our token
// is proof that this intent issued it. When it does not, all we can see is that
// a refund of some amount exists against the charge, which is equally consistent
// with a colleague issuing it manually. The second case returns Unknown.
//
// This is the same reasoning as the infrastructure domain, reached
// independently: attribution comes from an identifier we chose, never from the
// world happening to look right.
func (a RefundAction) Verify(ctx context.Context, args RefundArgs, idemKey, priorRef string) (verify.Verdict, error) {
	all, err := a.Ledger.All()
	if err != nil {
		return verify.Verdict{
			Disposition: verify.Unknown,
			Reason:      fmt.Sprintf("could not read the payment ledger: %v", err),
		}, nil
	}

	if a.Ledger.HonorIdempotencyKeys {
		for _, r := range all {
			if r.IdemToken == idemKey {
				outcome, _ := json.Marshal(map[string]any{
					"refund_id": r.ID, "charge_id": r.ChargeID,
					"amount_cents": r.AmountCents, "evidence": "provider_idempotency_key",
				})
				return verify.Verdict{
					Disposition: verify.Applied,
					ExternalRef: r.ID,
					Outcome:     outcome,
					Reason: fmt.Sprintf(
						"the provider's ledger holds refund %s carrying this intent's idempotency key, "+
							"so this call issued it", r.ID),
				}, nil
			}
		}
		return verify.Verdict{
			Disposition: verify.NotApplied,
			Reason: fmt.Sprintf(
				"no refund in the provider's ledger carries this intent's idempotency key; "+
					"the refund was not issued for charge %s", args.ChargeID),
		}, nil
	}

	// The provider does not record client tokens.
	cents, count, _ := a.Ledger.TotalRefunded(args.ChargeID)
	if count == 0 {
		return verify.Verdict{
			Disposition: verify.NotApplied,
			Reason: fmt.Sprintf(
				"the provider records no refund at all against charge %s, so this one was not issued",
				args.ChargeID),
		}, nil
	}
	outcome, _ := json.Marshal(map[string]any{
		"charge_id": args.ChargeID, "refunds_seen": count,
		"total_cents": cents, "evidence": "none",
	})
	return verify.Verdict{
		Disposition: verify.Unknown,
		Outcome:     outcome,
		Reason: fmt.Sprintf(
			"charge %s has %d refund(s) totalling %d cents, but this provider does not record "+
				"client idempotency keys, so none of them can be attributed to this intent. "+
				"A colleague refunding manually would look identical. Escalating rather than "+
				"recording a payment this agent may not have made.",
			args.ChargeID, count, cents),
	}, nil
}

// Effect returns nil. A refund changes a balance the payment provider owns, not
// state this system tracks, and the protocol does not require one.
func (a RefundAction) Effect(args RefundArgs) *verify.WorldEffect { return nil }

// Compile-time proof that a completely different domain satisfies the same
// contract, with no changes to the protocol, the coordinator or the reconciler.
var _ verify.TypedAction[RefundArgs] = RefundAction{}
