// Package verify defines the action registry and the ground-truth verification
// contract that Anchor's exactly-once protocol depends on.
//
// The central design constraint from the brief is that it must be impossible to
// register an action without supplying a verification strategy. That is enforced
// by the type system rather than by a runtime check: Verify is a method on the
// TypedAction interface, so an action type that does not implement it cannot be
// passed to Register and the program does not compile. There is no registration
// path that skips it.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Receipt is whatever the external system handed back when the action ran. It is
// captured in phase 2 and is the input the verifier uses to look up ground truth.
type Receipt struct {
	// ExternalRef is the external system's identifier for this operation. It is
	// the strongest evidence a verifier can have, because it is attributable to
	// this specific call rather than merely describing world state.
	ExternalRef string          `json:"external_ref"`
	Outcome     json.RawMessage `json:"outcome,omitempty"`
}

// Disposition is a verifier's conclusion about whether the action reached the
// external system.
type Disposition int

const (
	// Unknown means the verifier could not establish ground truth. This is the
	// most important value in this enum and it must never be treated as a
	// synonym for NotApplied.
	//
	// It exists because observing world state is not the same as attributing a
	// change to your own call. A verifier that reads "the cluster has 5 nodes"
	// cannot distinguish our scale committing from an operator scaling by hand.
	// Returning Applied there would manufacture a false history; returning
	// NotApplied would cause a double action. The reconciler leaves Unknown
	// intents PENDING and escalates them to an operator instead of guessing.
	Unknown Disposition = iota

	// Applied means the verifier confirmed, against the external system, that
	// this specific action took effect.
	Applied

	// NotApplied means the verifier confirmed the action did not take effect and
	// that it is safe to conclude the external world is untouched.
	NotApplied
)

func (d Disposition) String() string {
	switch d {
	case Applied:
		return "APPLIED"
	case NotApplied:
		return "NOT_APPLIED"
	default:
		return "UNKNOWN"
	}
}

// Verdict is a verifier's report on a single intent.
type Verdict struct {
	Disposition Disposition
	// ExternalRef is set when verification discovered the identifier for an
	// action whose originating process died before it could record one. This is
	// the orphaned-intent recovery path.
	ExternalRef string
	Outcome     json.RawMessage
	// Reason is operator-facing prose explaining how the verifier concluded what
	// it did. It is required for Unknown so that an escalation carries enough
	// context for a human to act on.
	Reason string
}

// TypedAction is the contract every action must satisfy. Execute and Verify are
// deliberately on the same interface: an author adding a new action type cannot
// get past the compiler without writing a way to check whether it happened.
type TypedAction[A any] interface {
	// Type is the stable action_type string persisted in action_intents. It
	// participates in the idempotency key, so changing it orphans existing rows.
	Type() string

	// Execute performs the external call. It runs in phase 2, outside any
	// database transaction, and may leave the world changed even when it
	// returns an error. That possibility is the entire reason Verify exists.
	Execute(ctx context.Context, args A) (*Receipt, error)

	// Verify queries the external system for ground truth about whether this
	// action took effect. priorRef is the external_ref recorded on the intent,
	// which is empty when the process died before recording one.
	//
	// Implementations must return Unknown rather than guessing. Preferring a
	// confident wrong answer here is how an agent's history diverges from
	// reality, which is the failure this project exists to prevent.
	Verify(ctx context.Context, args A, priorRef string) (Verdict, error)
}

// action is the type-erased form held by the registry, so that actions with
// different argument types can live in one map.
type action struct {
	typ     string
	execute func(ctx context.Context, raw json.RawMessage) (*Receipt, error)
	verify  func(ctx context.Context, raw json.RawMessage, priorRef string) (Verdict, error)
}

// Registry maps action_type strings to their executor and verifier.
type Registry struct {
	mu      sync.RWMutex
	actions map[string]action
}

func NewRegistry() *Registry {
	return &Registry{actions: make(map[string]action)}
}

// Register adds a TypedAction to the registry, erasing its argument type while
// preserving type-safe decoding at the call boundary.
//
// It is a free function rather than a method because Go does not permit type
// parameters on methods.
func Register[A any](r *Registry, a TypedAction[A]) error {
	typ := a.Type()
	if typ == "" {
		return fmt.Errorf("verify: action type string must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.actions[typ]; exists {
		// Duplicate registration would make dispatch depend on init order, and
		// the wrong verifier is worse than no verifier.
		return fmt.Errorf("verify: action type %q is already registered", typ)
	}

	decode := func(raw json.RawMessage) (A, error) {
		var args A
		if err := json.Unmarshal(raw, &args); err != nil {
			return args, fmt.Errorf("verify: decoding args for action %q: %w", typ, err)
		}
		return args, nil
	}

	r.actions[typ] = action{
		typ: typ,
		execute: func(ctx context.Context, raw json.RawMessage) (*Receipt, error) {
			args, err := decode(raw)
			if err != nil {
				return nil, err
			}
			return a.Execute(ctx, args)
		},
		verify: func(ctx context.Context, raw json.RawMessage, priorRef string) (Verdict, error) {
			args, err := decode(raw)
			if err != nil {
				return Verdict{}, err
			}
			v, err := a.Verify(ctx, args, priorRef)
			if err != nil {
				return Verdict{}, err
			}
			if v.Disposition == Unknown && v.Reason == "" {
				// An Unknown verdict becomes an operator escalation, and an
				// escalation with no explanation wastes the operator's time.
				return Verdict{}, fmt.Errorf(
					"verify: action %q returned Unknown without a Reason", typ)
			}
			return v, nil
		},
	}
	return nil
}

// MustRegister is Register for use in package initialization, where a
// registration failure is a programming error that should stop the process
// before it can take any action.
func MustRegister[A any](r *Registry, a TypedAction[A]) {
	if err := Register(r, a); err != nil {
		panic(err)
	}
}

var errUnregistered = "verify: no action registered for type %q; " +
	"every action_type in action_intents must have a verifier"

// Execute dispatches phase 2 for the given action type.
func (r *Registry) Execute(ctx context.Context, typ string, args json.RawMessage) (*Receipt, error) {
	r.mu.RLock()
	a, ok := r.actions[typ]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf(errUnregistered, typ)
	}
	return a.execute(ctx, args)
}

// Verify dispatches ground-truth verification for the given action type. The
// reconciler calls this for every intent it claims.
func (r *Registry) Verify(ctx context.Context, typ string, args json.RawMessage, priorRef string) (Verdict, error) {
	r.mu.RLock()
	a, ok := r.actions[typ]
	r.mu.RUnlock()
	if !ok {
		// An intent whose action type is not registered cannot be reconciled.
		// Failing loudly keeps it PENDING for an operator rather than silently
		// abandoning a possibly-applied change.
		return Verdict{}, fmt.Errorf(errUnregistered, typ)
	}
	return a.verify(ctx, args, priorRef)
}

// Types lists registered action types in sorted order, for startup logging and
// for the observability panel.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.actions))
	for t := range r.actions {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
