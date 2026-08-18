// Package bedrock implements Anchor's embedding path on Amazon Bedrock.
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/virajchogle/anchor/internal/store"
)

// ModelTitanV2 is Amazon Titan Text Embeddings V2.
const ModelTitanV2 = "amazon.titan-embed-text-v2:0"

// Embedder turns incident text into vectors for CockroachDB's vector index.
//
// Dimensions are pinned to store.Dims (1024) and asserted on every response.
// Titan V2 can emit 256, 512, or 1024, and the schema declares VECTOR(1024), so
// a silently misconfigured model would otherwise fail deep inside a phase 3
// transaction rather than at the call site.
type Embedder struct {
	client *bedrockruntime.Client
	model  string

	// Normalize asks Titan for unit-length vectors. Cosine distance is
	// scale-invariant so this does not change ranking, but it keeps the values
	// comparable with the centroids consolidation computes locally.
	normalize bool

	mu    sync.Mutex
	calls int
}

// New builds an Embedder from the ambient AWS configuration.
func New(ctx context.Context, region string) (*Embedder, error) {
	opts := []func(*config.LoadOptions) error{}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bedrock: loading AWS config: %w", err)
	}
	return &Embedder{
		client:    bedrockruntime.NewFromConfig(cfg),
		model:     ModelTitanV2,
		normalize: true,
	}, nil
}

type titanRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

type titanResponse struct {
	Embedding           []float32 `json:"embedding"`
	InputTextTokenCount int       `json:"inputTextTokenCount"`
}

// Embed returns the embedding for text.
func (e *Embedder) Embed(ctx context.Context, text string) (store.Vector, error) {
	if text == "" {
		return nil, fmt.Errorf("bedrock: refusing to embed empty text")
	}

	body, err := json.Marshal(titanRequest{
		InputText:  text,
		Dimensions: store.Dims,
		Normalize:  e.normalize,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := e.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(e.model),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoking %s: %w", e.model, err)
	}

	var resp titanResponse
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		return nil, fmt.Errorf("bedrock: decoding response: %w", err)
	}

	vec := store.Vector(resp.Embedding)
	// Assert width here rather than letting the database reject it later, so a
	// model misconfiguration names itself instead of surfacing as a constraint
	// violation inside an action commit.
	if err := vec.Validate(); err != nil {
		return nil, fmt.Errorf("bedrock: %s returned the wrong width: %w", e.model, err)
	}

	e.mu.Lock()
	e.calls++
	e.mu.Unlock()

	return vec, nil
}

// Calls reports how many model invocations have been made, for the panel and
// for cost accounting during benchmark runs.
func (e *Embedder) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// Model reports the model id in use, surfaced in structured logs.
func (e *Embedder) Model() string { return e.model }
