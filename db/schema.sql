-- Anchor: transactional memory for acting agents.
--
-- Two deviations from the original design, both verified against CockroachDB
-- v26.2.5 rather than assumed. See docs/protocol.md for the reasoning.
--
--   1. episodes.expires_at drives row-level TTL. A naive TTL on created_at is
--      accepted at DDL time and then fails at run time with 23503, because
--      action_intents holds an FK to episodes. Phase 3 nulls expires_at in the
--      same transaction that binds an action to a memory, so any episode with a
--      committed action becomes permanent and is never eligible for expiry.
--      Memory decays; the record of what the agent did to the world does not.
--
--   2. One episode per incident. The episode is created at incident open so that
--      idem_key has an episode_id to hash. Phase 3 UPDATEs that same row rather
--      than inserting a second one, which keeps recall and consolidation reading
--      whole incidents instead of fragments.

CREATE TABLE IF NOT EXISTS agents (
  agent_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name          STRING NOT NULL,
  scope         STRING[] NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- status lifecycle: 'open' -> 'resolved' -> 'archived'
--   open      incident in flight, symptom embedded, no action committed yet
--   resolved  an action committed against it; expires_at is NULL, so it is pinned
--   archived  consolidated into a playbook; retained but excluded from recall
CREATE TABLE IF NOT EXISTS episodes (
  episode_id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  scope_key     STRING NOT NULL,
  status        STRING NOT NULL,
  symptom       STRING NOT NULL,
  narrative     STRING NOT NULL,
  outcome       STRING,
  salience      FLOAT NOT NULL DEFAULT 0.5,
  embedding     VECTOR(1024) NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- NULL means pinned forever. Set at insert, cleared by phase 3.
  expires_at    TIMESTAMPTZ,
  CONSTRAINT episodes_status_valid
    CHECK (status IN ('open', 'resolved', 'archived'))
);

-- The opclass must match the operator used at query time. Searching this index
-- with <-> instead of <=> silently degrades to a full scan with no warning.
-- Only prefix columns may appear in WHERE; a predicate on salience or created_at
-- also silently degrades to a full scan. Both are asserted via EXPLAIN in the
-- integration tests. Apply recency and salience in application code post-retrieval.
CREATE VECTOR INDEX IF NOT EXISTS idx_episodes_recall
  ON episodes (scope_key, status, embedding vector_cosine_ops);

ALTER TABLE episodes SET (
  ttl_expiration_expression = 'expires_at',
  ttl_job_cron = '@daily'
);

CREATE TABLE IF NOT EXISTS playbooks (
  playbook_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  scope_key     STRING NOT NULL,
  status        STRING NOT NULL,
  title         STRING NOT NULL,
  steps         JSONB NOT NULL,
  derived_from  UUID[] NOT NULL,
  confidence    FLOAT NOT NULL,
  embedding     VECTOR(1024) NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE VECTOR INDEX IF NOT EXISTS idx_playbooks_recall
  ON playbooks (scope_key, status, embedding vector_cosine_ops);

-- The durable intent log. This table is append-mostly and never TTL'd: it is the
-- agent's record of what it did to the outside world.
--
-- state machine:
--   PENDING    intent recorded, external call may or may not have happened
--   COMMITTED  verified against the external system, outcome recorded
--   FAILED     verified as not-applied, or unrecoverable; surfaced to an operator
CREATE TABLE IF NOT EXISTS action_intents (
  idem_key      STRING PRIMARY KEY,
  episode_id    UUID NOT NULL REFERENCES episodes(episode_id),
  agent_id      UUID NOT NULL REFERENCES agents(agent_id),
  action_type   STRING NOT NULL,
  args          JSONB NOT NULL,
  state         STRING NOT NULL,
  lease_owner   UUID,
  lease_expires TIMESTAMPTZ,
  external_ref  STRING,
  outcome       JSONB,
  attempts      INT NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at   TIMESTAMPTZ,
  CONSTRAINT intents_state_valid
    CHECK (state IN ('PENDING', 'COMMITTED', 'FAILED')),
  -- A committed intent must carry evidence of what happened. This is a schema
  -- level guard against the protocol's worst failure mode: marking an action
  -- done without a verifier having confirmed anything.
  CONSTRAINT intents_committed_has_evidence
    CHECK (state <> 'COMMITTED' OR (outcome IS NOT NULL AND resolved_at IS NOT NULL))
);

-- Drives the reconciler's claim scan: unresolved intents whose lease has lapsed.
CREATE INDEX IF NOT EXISTS idx_intents_unresolved
  ON action_intents (state, lease_expires) WHERE state = 'PENDING';

CREATE TABLE IF NOT EXISTS managed_clusters (
  cluster_id    STRING PRIMARY KEY,
  scope_key     STRING NOT NULL,
  desired_nodes INT NOT NULL,
  last_action   STRING,
  version       INT NOT NULL DEFAULT 0
);
