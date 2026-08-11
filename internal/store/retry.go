package store

import (
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Outcome classifies a database error into the only three responses the protocol
// permits. The distinction between Retryable and Ambiguous is the entire reason
// this project exists, so it is worth stating precisely.
type Outcome int

const (
	// Fatal means the unit of work failed and the world was not changed by the
	// database. Surface it.
	Fatal Outcome = iota

	// Retryable means CockroachDB has already aborted the transaction, so no
	// part of it took effect and replaying it is safe. This is 40001.
	//
	// Safe to replay includes phase 3, which runs after the external call has
	// already happened: replaying phase 3 re-runs SQL only, never the external
	// call, so the action cannot happen twice on this path.
	Retryable

	// Ambiguous means we do not know whether the transaction committed. The
	// connection dropped, the node shut down, or the server explicitly reported
	// completion-unknown. The commit may have landed and we simply never heard.
	//
	// This is the case a naive retry gets wrong. Replaying the unit of work
	// means replaying the external call, which double-acts. Every Ambiguous
	// error routes to the reconciler, which establishes ground truth from the
	// external system before resolving anything.
	Ambiguous
)

func (o Outcome) String() string {
	switch o {
	case Retryable:
		return "RETRYABLE"
	case Ambiguous:
		return "AMBIGUOUS"
	default:
		return "FATAL"
	}
}

// Classify maps a database error to its protocol response.
func Classify(err error) Outcome {
	if err == nil {
		return Fatal
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001":
			// serialization_failure. CockroachDB aborted the transaction, so
			// nothing in it took effect.
			return Retryable

		case "40003":
			// statement_completion_unknown. CockroachDB's explicit signal that
			// it cannot tell us whether the statement committed. This is the
			// textbook ambiguous commit and must never be replayed blindly.
			return Ambiguous

		case "57P01":
			// admin_shutdown. The node went away mid-transaction; the commit may
			// or may not have replicated.
			return Ambiguous
		}

		// Class 08, connection exception. The connection dropped at a point we
		// cannot reason about, so the commit status is unknown.
		if len(pgErr.Code) >= 2 && pgErr.Code[:2] == "08" {
			return Ambiguous
		}
		return Fatal
	}

	// A connection-level failure with no SQLSTATE at all is the worst case: the
	// request may have reached the server and committed without us seeing the
	// response. Treat it as ambiguous rather than assuming it failed.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return Ambiguous
	}
	if pgconn.SafeToRetry(err) {
		// pgx guarantees the request never reached the server.
		return Retryable
	}
	return Ambiguous
}

// Backoff returns the delay before retry attempt n, counting from 1.
//
// Jitter is not decoration. Concurrent agents contending on the same scope abort
// each other with 40001; without jitter they retry in lockstep and collide
// again on the same schedule, which converts one contention event into a
// sustained livelock. The benchmark harness records retry rate with and without
// it.
func Backoff(n int) time.Duration {
	const (
		base = 5 * time.Millisecond
		max  = 2 * time.Second
	)
	if n < 1 {
		n = 1
	}
	d := float64(base) * math.Pow(2, float64(n-1))
	if d > float64(max) {
		d = float64(max)
	}
	// Full jitter: uniform over [0, d). Decorrelates colliding writers better
	// than the equal-jitter variant at the contention levels we benchmark.
	return time.Duration(rand.Float64() * d)
}
