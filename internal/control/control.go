// Package control implements the architecture Anchor argues against, so the
// argument can be tested instead of asserted.
//
// This is the standard shape of an agent with memory in 2026: a relational
// database for operational state, a separate vector database for embeddings,
// and no durable intent log. It is written to be a fair representative of that
// design rather than a strawman. It retries on failure, it records what it did,
// and it stores good embeddings. It simply cannot make two promises at once:
//
//  1. that an action happened exactly once, and
//  2. that its memory of the action agrees with reality.
//
// Both failures are reproduced by tests in this package.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/store"
)

// VectorStore stands in for a managed vector database such as Pinecone.
//
// The important property is not how it stores vectors, it is that writing to it
// is a second network call in a second transaction. It can fail on its own,
// after the relational write has already committed, and nothing rolls back.
type VectorStore struct {
	mu   sync.Mutex
	path string

	// FailNextWrite simulates the vector store being unavailable, which is the
	// ordinary partial-failure every two-store architecture has to survive.
	FailNextWrite bool
}

func NewVectorStore(path string) *VectorStore { return &VectorStore{path: path} }

// Record is one memory in the separate vector store.
type Record struct {
	EpisodeID string       `json:"episode_id"`
	Symptom   string       `json:"symptom"`
	Narrative string       `json:"narrative"`
	Embedding store.Vector `json:"embedding"`
}

func (v *VectorStore) Upsert(rec Record) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.FailNextWrite {
		v.FailNextWrite = false
		return fmt.Errorf("control: vector store unavailable")
	}

	recs, err := v.readAll()
	if err != nil {
		return err
	}
	for i := range recs {
		if recs[i].EpisodeID == rec.EpisodeID {
			recs[i] = rec
			return v.writeAll(recs)
		}
	}
	return v.writeAll(append(recs, rec))
}

func (v *VectorStore) Count() (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	recs, err := v.readAll()
	return len(recs), err
}

func (v *VectorStore) readAll() ([]Record, error) {
	b, err := os.ReadFile(v.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []Record
	if len(b) == 0 {
		return nil, nil
	}
	return recs, json.Unmarshal(b, &recs)
}

func (v *VectorStore) writeAll(recs []Record) error {
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	return os.WriteFile(v.path, b, 0o600)
}

// ControlSchema is the relational half. There is no intent table, because the
// architecture being modelled does not have one: the action is taken, and then
// the result is recorded.
const ControlSchema = `
CREATE TABLE IF NOT EXISTS control_runs (
  run_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  episode_id  UUID NOT NULL,
  action_type STRING NOT NULL,
  args        JSONB NOT NULL,
  external_ref STRING,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS control_clusters (
  cluster_id    STRING PRIMARY KEY,
  desired_nodes INT NOT NULL,
  version       INT NOT NULL DEFAULT 0
);`

// Agent is the control implementation.
type Agent struct {
	DB     *pgxpool.Pool
	Vector *VectorStore
}

// HasActedBefore is the only defence this architecture has against repeating an
// action: look for a record that it already ran.
//
// The gap is structural, not a bug. The record is written AFTER the external
// call returns, so a process that dies between the call and the write leaves no
// evidence, and the next run cannot tell "never happened" from "happened and we
// crashed". Anchor closes that gap by writing the intent BEFORE acting.
func (a *Agent) HasActedBefore(ctx context.Context, episodeID uuid.UUID, actionType string) (bool, error) {
	var n int
	err := a.DB.QueryRow(ctx,
		`SELECT count(*) FROM control_runs WHERE episode_id=$1 AND action_type=$2`,
		episodeID, actionType).Scan(&n)
	return n > 0, err
}

// RecordRun writes the relational half after the action has been performed.
func (a *Agent) RecordRun(ctx context.Context, episodeID uuid.UUID, actionType string, args any, externalRef string) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = a.DB.Exec(ctx,
		`INSERT INTO control_runs (episode_id, action_type, args, external_ref) VALUES ($1,$2,$3,$4)`,
		episodeID, actionType, raw, externalRef)
	return err
}

// UpdateCluster writes world state, in its own transaction.
func (a *Agent) UpdateCluster(ctx context.Context, clusterID string, nodes int) error {
	_, err := a.DB.Exec(ctx,
		`UPSERT INTO control_clusters (cluster_id, desired_nodes, version)
		 VALUES ($1, $2, coalesce((SELECT version FROM control_clusters WHERE cluster_id=$1), 0) + 1)`,
		clusterID, nodes)
	return err
}

// WriteMemory writes the embedding to the separate vector store.
//
// This is the second transaction. It has already been preceded by a committed
// relational write, so when it fails the two stores disagree and no rollback is
// available. That divergence is the second thing this package demonstrates.
func (a *Agent) WriteMemory(rec Record) error { return a.Vector.Upsert(rec) }
