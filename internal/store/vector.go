package store

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// Vector bridges Go float slices and CockroachDB's VECTOR type.
//
// pgx has no built-in codec for VECTOR, and the failure is not obvious. Passing
// a []float32 directly makes pgx encode it as a PostgreSQL array literal,
// {1,2,3}, and CockroachDB rejects it:
//
//	malformed vector literal: Vector contents must start with "[" and end with "]"
//
// Scanning is worse, because it fails only at read time with an opaque array
// parse error. Implementing pgtype.TextValuer and pgtype.TextScanner makes pgx
// round-trip the value as a text literal in the bracket form CockroachDB wants,
// so callers can pass and scan Vector transparently with no SQL casts.
type Vector []float32

// TextValue satisfies pgtype.TextValuer, used when sending a parameter.
func (v Vector) TextValue() (pgtype.Text, error) {
	return pgtype.Text{String: v.String(), Valid: true}, nil
}

// ScanText satisfies pgtype.TextScanner, used when reading a result column.
func (v *Vector) ScanText(t pgtype.Text) error {
	if !t.Valid {
		*v = nil
		return nil
	}
	parsed, err := ParseVector(t.String)
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}

// String renders the bracket form CockroachDB expects: [1,2,3].
//
// Components are formatted with -1 precision so the shortest representation that
// round-trips is used. Embeddings are float32 on the wire, and formatting them
// through float64 defaults would both bloat the literal and introduce digits
// that were never in the original value.
func (v Vector) String() string {
	var b strings.Builder
	b.Grow(len(v)*12 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// ParseVector reads the bracket form back into a slice.
func ParseVector(s string) (Vector, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("store: malformed vector literal %q: expected [..]", truncate(s))
	}
	body := s[1 : len(s)-1]
	if strings.TrimSpace(body) == "" {
		return Vector{}, nil
	}

	parts := strings.Split(body, ",")
	out := make(Vector, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("store: malformed vector component %d (%q): %w", i, strings.TrimSpace(p), err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// Dims is the embedding width this project is built around: Titan Text
// Embeddings V2 configured to 1024. The schema declares VECTOR(1024), so a
// mismatch is rejected by the database rather than silently stored.
const Dims = 1024

// Validate checks width before a write, so a misconfigured embedding model is
// caught at the call site with a clear message instead of as a constraint error
// from inside a transaction.
func (v Vector) Validate() error {
	if len(v) != Dims {
		return fmt.Errorf("store: embedding has %d dimensions, schema requires %d", len(v), Dims)
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
