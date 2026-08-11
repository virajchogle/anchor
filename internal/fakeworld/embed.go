package fakeworld

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"

	"github.com/virajchogle/anchor/internal/store"
)

// HashEmbedder produces deterministic embeddings without a network call.
//
// It exists so the chaos test exercises the real transactional path, including
// the VECTOR write, without depending on Bedrock being reachable or on model
// availability. It is not a semantic embedding and is never used outside tests
// and the chaos demo; the production path is the Bedrock Titan embedder.
//
// Vectors are L2-normalized so cosine distance behaves sensibly, which keeps
// index-usage assertions meaningful even on synthetic data.
type HashEmbedder struct{}

func (HashEmbedder) Embed(_ context.Context, text string) (store.Vector, error) {
	v := make(store.Vector, store.Dims)

	// Expand the digest deterministically to fill the full width.
	var sum float64
	seed := sha256.Sum256([]byte(text))
	for i := 0; i < store.Dims; i++ {
		block := sha256.Sum256(append(seed[:], byte(i), byte(i>>8)))
		bits := binary.BigEndian.Uint32(block[:4])
		// Map to [-1, 1).
		f := float64(bits)/float64(1<<31) - 1.0
		v[i] = float32(f)
		sum += f * f
	}

	norm := math.Sqrt(sum)
	if norm > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
	}
	return v, nil
}
