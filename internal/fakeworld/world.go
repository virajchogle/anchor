// Package fakeworld is a file-backed stand-in for a non-idempotent external API,
// used by the chaos test and the control implementation.
//
// It is deliberately append-only. A double-action leaves two records rather than
// overwriting one, so "did this act twice" is a question the test can answer
// from evidence instead of inference. It lives on disk rather than in memory
// because the chaos test kills and restarts a real process, and the world has to
// outlive the process that changed it, exactly as a real cloud API would.
package fakeworld

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

// Op is one recorded external operation.
type Op struct {
	ExternalRef string    `json:"external_ref"`
	ActionType  string    `json:"action_type"`
	ClusterID   string    `json:"cluster_id"`
	Nodes       int       `json:"nodes"`
	At          time.Time `json:"at"`

	// IdemToken is the client-supplied idempotency key. A real API that accepts
	// one is what makes attribution possible: the verifier can ask "is there an
	// operation carrying MY token" rather than the much weaker "does the world
	// happen to look the way I wanted".
	IdemToken string `json:"idem_token"`
}

// World is an append-only operation log on disk.
type World struct {
	mu   sync.Mutex
	path string
}

func New(path string) *World { return &World{path: path} }

// Path is the on-disk location of the operation log.
func (w *World) Path() string { return w.path }

// Apply appends an operation. It does not deduplicate, because the external
// systems this stands in for do not either. That is the whole problem.
func (w *World) Apply(op Op) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(op)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	// Durability matters here: the chaos test kills the process immediately
	// after this returns, and an operation lost to a buffer would make the world
	// look untouched when it was not, quietly turning a real bug into a pass.
	return f.Sync()
}

// Ops reads the full operation log.
func (w *World) Ops() ([]Op, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Op
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var op Op
		if err := json.Unmarshal(sc.Bytes(), &op); err != nil {
			return nil, fmt.Errorf("fakeworld: corrupt op log line: %w", err)
		}
		out = append(out, op)
	}
	return out, sc.Err()
}

// CountByToken reports how many operations carry a given idempotency token.
// The chaos test asserts this is exactly 1.
func (w *World) CountByToken(token string) (int, error) {
	ops, err := w.Ops()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, op := range ops {
		if op.IdemToken == token {
			n++
		}
	}
	return n, nil
}

// ScaleArgs are the arguments to the scale_cluster action.
type ScaleArgs struct {
	ClusterID string `json:"cluster_id"`
	Nodes     int    `json:"nodes"`
	Reason    string `json:"reason"`
}

// ScaleAction is a non-idempotent scale operation against the fake world.
//
// It satisfies verify.TypedAction, which the compiler enforces: without Verify
// and Effect it could not be registered at all.
type ScaleAction struct {
	World *World
	// CrashAfterExecute makes the process die between phase 2 and phase 3,
	// which is the precise window the reconciler exists to cover.
	CrashAfterExecute bool
}

func (a ScaleAction) Type() string { return "scale_cluster" }

func (a ScaleAction) Execute(ctx context.Context, args ScaleArgs, idemKey string) (*verify.Receipt, error) {
	ref := fmt.Sprintf("op-%s", idemKey[:12])
	op := Op{
		ExternalRef: ref,
		ActionType:  a.Type(),
		ClusterID:   args.ClusterID,
		Nodes:       args.Nodes,
		At:          time.Now().UTC(),
		IdemToken:   idemKey,
	}
	if err := a.World.Apply(op); err != nil {
		return nil, err
	}

	if a.CrashAfterExecute {
		// Hard exit. No deferred functions, no flush, no chance to record the
		// receipt in the database. This is what a real crash looks like and it
		// is the only honest way to test the recovery path.
		os.Exit(9)
	}

	outcome, _ := json.Marshal(map[string]any{"nodes": args.Nodes, "cluster_id": args.ClusterID})
	return &verify.Receipt{ExternalRef: ref, Outcome: outcome}, nil
}

// Verify establishes ground truth by looking for an operation carrying this
// intent's idempotency token.
//
// Note what it does NOT do: it does not check whether the cluster currently has
// the requested node count. That would be unattributable, because an operator
// could have scaled it by hand. Searching for our own token is what makes the
// Applied verdict honest.
func (a ScaleAction) Verify(ctx context.Context, args ScaleArgs, idemKey, priorRef string) (verify.Verdict, error) {
	ops, err := a.World.Ops()
	if err != nil {
		// We could not read the external system, so we know nothing. Unknown,
		// never NotApplied.
		return verify.Verdict{
			Disposition: verify.Unknown,
			Reason:      fmt.Sprintf("could not read external operation log: %v", err),
		}, nil
	}

	for _, op := range ops {
		if op.IdemToken == idemKey || (priorRef != "" && op.ExternalRef == priorRef) {
			outcome, _ := json.Marshal(map[string]any{"nodes": op.Nodes, "cluster_id": op.ClusterID})
			return verify.Verdict{
				Disposition: verify.Applied,
				ExternalRef: op.ExternalRef,
				Outcome:     outcome,
				Reason: fmt.Sprintf("found external operation %s carrying this intent's idempotency token",
					op.ExternalRef),
			}, nil
		}
	}

	return verify.Verdict{
		Disposition: verify.NotApplied,
		Reason:      "no external operation carries this intent's idempotency token",
	}, nil
}

func (a ScaleAction) Effect(args ScaleArgs) *verify.ClusterEffect {
	nodes := args.Nodes
	return &verify.ClusterEffect{
		ClusterID:    args.ClusterID,
		DesiredNodes: &nodes,
		LastAction:   a.Type(),
	}
}
