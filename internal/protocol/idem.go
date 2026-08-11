// Package protocol implements Anchor's exactly-once action protocol.
//
// The idempotency key is the load-bearing part. Two agents that decide to take
// the same logical action against the same incident must derive byte-identical
// keys, or the INSERT ... ON CONFLICT DO NOTHING in phase 1 fails to deduplicate
// and the action happens twice. Everything in this file exists to make that
// derivation deterministic.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// IdemKey derives the primary key of an action_intents row.
//
// Determinism requirements, all of which are covered by tests:
//   - Map key ordering in the caller's args must not affect the result.
//   - Numeric spelling (1 vs 1.0 vs 1e0) must not affect the result.
//   - The three inputs must not be ambiguously concatenable, so each is written
//     with an explicit length prefix. Without this, ("ab","c") and ("a","bc")
//     would hash identically.
func IdemKey(episodeID, actionType string, args any) (string, error) {
	canon, err := CanonicalJSON(args)
	if err != nil {
		return "", fmt.Errorf("canonicalize args: %w", err)
	}

	h := sha256.New()
	for _, field := range [][]byte{[]byte(episodeID), []byte(actionType), canon} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(field)))
		h.Write(n[:])
		h.Write(field)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CanonicalJSON serializes v to a deterministic byte sequence.
//
// This is a deliberately strict subset of RFC 8785. It sorts object keys by
// byte order and rejects non-ASCII keys outright rather than silently producing
// an ordering that disagrees with JCS's UTF-16 code unit ordering. Action
// argument names are identifiers, so the restriction costs nothing and removes a
// class of ambiguity that would otherwise be invisible until it caused a
// duplicate action in production.
func CanonicalJSON(v any) ([]byte, error) {
	// Round-trip through encoding/json so that structs, maps, and custom
	// json.Marshaler implementations all reduce to the same generic shape
	// before canonicalization.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // preserve literals so we control number formatting ourselves
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := writeCanonical(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")

	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}

	case json.Number:
		return writeCanonicalNumber(buf, t)

	case string:
		writeCanonicalString(buf, t)

	case []any:
		buf.WriteByte('[')
		for i, elem := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')

	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if !isASCII(k) {
				return fmt.Errorf("canonical json: non-ASCII object key %q; "+
					"action argument names must be ASCII so key ordering is unambiguous", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)

		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')

	default:
		return fmt.Errorf("canonical json: unexpected type %T after generic decode", v)
	}
	return nil
}

// writeCanonicalNumber collapses every spelling of a value to one form. Integers
// are written as integers; everything else uses the shortest representation that
// round-trips. So 1, 1.0, and 1e0 all canonicalize to "1".
func writeCanonicalNumber(buf *bytes.Buffer, n json.Number) error {
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}

	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return fmt.Errorf("canonical json: unparseable number %q: %w", n.String(), err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("canonical json: %v cannot appear in an idempotency key", f)
	}
	// A float that happens to hold an integral value must agree with the integer
	// path above, otherwise 2.0 and 2 would produce different keys.
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		buf.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// writeCanonicalString emits a minimally escaped JSON string. encoding/json is
// not used here because it HTML-escapes <, >, and & by default, which is a
// presentation concern that must not leak into a hash input.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

func isASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > 127 }) < 0
}
