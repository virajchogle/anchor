package store

import (
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestVector_UsesBracketFormNotArrayForm(t *testing.T) {
	// The whole point of this type. PostgreSQL array form {1,2,3} is what pgx
	// produces for a bare []float32, and CockroachDB rejects it.
	got := Vector{1, 2.5, -3}.String()
	if got != "[1,2.5,-3]" {
		t.Fatalf("got %q, want bracket form", got)
	}
	if strings.ContainsAny(got, "{}") {
		t.Errorf("emitted PostgreSQL array form, which CockroachDB rejects: %s", got)
	}
}

func TestVector_RoundTrip(t *testing.T) {
	in := Vector{0, 1, -1, 0.15625, 3.4028235e38, 1e-8}

	var out Vector
	tv, err := in.TextValue()
	if err != nil {
		t.Fatal(err)
	}
	if err := out.ScanText(tv); err != nil {
		t.Fatal(err)
	}

	if len(out) != len(in) {
		t.Fatalf("length changed: %d -> %d", len(in), len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("component %d not exact: %v -> %v", i, in[i], out[i])
		}
	}
}

// Formatting through float64 defaults would introduce digits that were never in
// the float32 value, bloating the literal and losing exactness on round-trip.
func TestVector_Float32Precision(t *testing.T) {
	v := Vector{0.1}
	if got := v.String(); got != "[0.1]" {
		t.Errorf("float32 0.1 should format as 0.1, got %s", got)
	}
	var out Vector
	if err := out.ScanText(pgtype.Text{String: "[0.1]", Valid: true}); err != nil {
		t.Fatal(err)
	}
	if out[0] != float32(0.1) {
		t.Errorf("round-trip changed value: %v", out[0])
	}
}

func TestParseVector_Errors(t *testing.T) {
	for name, in := range map[string]string{
		"array form":     "{1,2,3}",
		"unterminated":   "[1,2,3",
		"empty string":   "",
		"bad component":  "[1,abc,3]",
		"stray brackets": "]1,2,3[",
	} {
		if _, err := ParseVector(in); err == nil {
			t.Errorf("%s: expected error for %q", name, in)
		}
	}
}

func TestParseVector_Empty(t *testing.T) {
	v, err := ParseVector("[]")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("expected empty vector, got %d components", len(v))
	}
}

func TestVector_NullScansToNil(t *testing.T) {
	v := Vector{1, 2, 3}
	if err := v.ScanText(pgtype.Text{Valid: false}); err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Errorf("NULL should scan to nil, got %v", v)
	}
}

func TestVector_ValidateWidth(t *testing.T) {
	if err := make(Vector, Dims).Validate(); err != nil {
		t.Errorf("correct width rejected: %v", err)
	}
	// A misconfigured embedding model is the realistic cause here, so the error
	// must name both widths.
	err := make(Vector, 512).Validate()
	if err == nil {
		t.Fatal("wrong width accepted")
	}
	if !strings.Contains(err.Error(), "512") || !strings.Contains(err.Error(), "1024") {
		t.Errorf("error should name both widths, got: %v", err)
	}
}

func TestVector_NonFiniteRoundTrip(t *testing.T) {
	// CockroachDB rejects these at the column level, but the formatter must not
	// silently emit something that parses back as a different value.
	v := Vector{float32(math.Inf(1))}
	out, err := ParseVector(v.String())
	if err != nil {
		return // rejecting is acceptable
	}
	if !math.IsInf(float64(out[0]), 1) {
		t.Errorf("+Inf round-tripped to %v", out[0])
	}
}
