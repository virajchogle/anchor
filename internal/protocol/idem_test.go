package protocol

import (
	"math"
	"strings"
	"testing"
)

const (
	ep  = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	ep2 = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
)

func mustKey(t *testing.T, episode, action string, args any) string {
	t.Helper()
	k, err := IdemKey(episode, action, args)
	if err != nil {
		t.Fatalf("IdemKey(%q, %q, %v): %v", episode, action, args, err)
	}
	return k
}

// The property the whole protocol rests on: the same logical action derives the
// same key no matter how the caller happened to spell it.
func TestIdemKey_InsensitiveToArgSpelling(t *testing.T) {
	type scaleArgs struct {
		ClusterID string `json:"cluster_id"`
		Nodes     int    `json:"nodes"`
		Reason    string `json:"reason"`
	}

	want := mustKey(t, ep, "scale_cluster", map[string]any{
		"cluster_id": "c-1", "nodes": 5, "reason": "high p99",
	})

	cases := map[string]any{
		"reordered keys": map[string]any{
			"reason": "high p99", "nodes": 5, "cluster_id": "c-1",
		},
		"float spelling of an integer": map[string]any{
			"cluster_id": "c-1", "nodes": 5.0, "reason": "high p99",
		},
		"exponent spelling of an integer": map[string]any{
			"cluster_id": "c-1", "nodes": 5e0, "reason": "high p99",
		},
		"struct instead of map": scaleArgs{
			ClusterID: "c-1", Nodes: 5, Reason: "high p99",
		},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if got := mustKey(t, ep, "scale_cluster", args); got != want {
				t.Errorf("key diverged for equivalent args\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// Different logical actions must not collide.
func TestIdemKey_DistinguishesInputs(t *testing.T) {
	base := mustKey(t, ep, "scale_cluster", map[string]any{"nodes": 5})

	cases := map[string]string{
		"different episode": mustKey(t, ep2, "scale_cluster", map[string]any{"nodes": 5}),
		"different action":  mustKey(t, ep, "create_backup", map[string]any{"nodes": 5}),
		"different args":    mustKey(t, ep, "scale_cluster", map[string]any{"nodes": 6}),
		"added arg":         mustKey(t, ep, "scale_cluster", map[string]any{"nodes": 5, "x": nil}),
	}

	for name, got := range cases {
		if got == base {
			t.Errorf("%s collided with base key %s", name, base)
		}
	}
}

// Without length-prefixed hashing, ("ab","c") and ("a","bc") hash identically.
// This is the regression test for that.
func TestIdemKey_ConcatenationIsUnambiguous(t *testing.T) {
	a := mustKey(t, "ab", "c", nil)
	b := mustKey(t, "a", "bc", nil)
	if a == b {
		t.Fatalf("field boundaries are ambiguous: (%q,%q) and (%q,%q) both hash to %s",
			"ab", "c", "a", "bc", a)
	}
}

func TestCanonicalJSON_SortsNestedKeys(t *testing.T) {
	got, err := CanonicalJSON(map[string]any{
		"z": 1,
		"a": map[string]any{"n": 2, "b": 1},
		"m": []any{map[string]any{"y": 1, "x": 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"b":1,"n":2},"m":[{"x":2,"y":1}],"z":1}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// Array order is meaningful and must be preserved, unlike object key order.
func TestCanonicalJSON_PreservesArrayOrder(t *testing.T) {
	got, err := CanonicalJSON([]any{3, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[3,1,2]` {
		t.Errorf("array order not preserved: %s", got)
	}
}

// encoding/json HTML-escapes <, >, and & by default. That is a presentation
// concern and must never reach a hash input.
func TestCanonicalJSON_NoHTMLEscaping(t *testing.T) {
	got, err := CanonicalJSON(map[string]any{"q": "a<b&c>d"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `\u00`) {
		t.Errorf("HTML escaping leaked into canonical form: %s", got)
	}
	if string(got) != `{"q":"a<b&c>d"}` {
		t.Errorf("got %s", got)
	}
}

func TestCanonicalJSON_EscapesControlCharacters(t *testing.T) {
	got, err := CanonicalJSON(map[string]any{"s": "a\nb\tc\x01"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"s":"a\nb\tc\u0001"}` {
		t.Errorf("got %s", got)
	}
}

// Non-ASCII keys are rejected rather than sorted under an ordering that would
// disagree with RFC 8785. Failing loudly beats a duplicate action later.
func TestCanonicalJSON_RejectsNonASCIIKeys(t *testing.T) {
	if _, err := CanonicalJSON(map[string]any{"café": 1}); err == nil {
		t.Fatal("expected non-ASCII key to be rejected")
	}
	// Non-ASCII values are fine; only keys participate in ordering.
	if _, err := CanonicalJSON(map[string]any{"city": "café"}); err != nil {
		t.Fatalf("non-ASCII value should be accepted: %v", err)
	}
}

func TestCanonicalJSON_RejectsNonFiniteNumbers(t *testing.T) {
	for name, f := range map[string]float64{
		"NaN":  math.NaN(),
		"+Inf": math.Inf(1),
		"-Inf": math.Inf(-1),
	} {
		// encoding/json rejects these at marshal time; assert we surface an
		// error either way rather than emitting an unparseable key.
		if _, err := CanonicalJSON(map[string]any{"v": f}); err == nil {
			t.Errorf("%s was accepted into a canonical form", name)
		}
	}
}

// Guards against an accidental change to the hashing construction. If this
// fails, every previously written idem_key in the database is orphaned, so the
// value is pinned deliberately.
func TestIdemKey_IsStable(t *testing.T) {
	got := mustKey(t, ep, "scale_cluster", map[string]any{
		"cluster_id": "c-1", "nodes": 5, "reason": "high p99",
	})
	const want = "75742cfc935a73c99fc3fd7853cce5b03fd8c19bec269872aeefb80b12144a50"
	if got != want {
		t.Errorf("hash construction changed.\n got: %s\nwant: %s\n"+
			"If this change is intentional, existing intent rows are orphaned.", got, want)
	}
}
