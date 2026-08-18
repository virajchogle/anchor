# Anchor — Devpost submission

## Inline summary

Transactional memory for agents that act. Anchor is an autonomous on-call agent
for CockroachDB Cloud whose durable intent log commits in the same transaction as
its memory write, so its record of what it did cannot diverge from what actually
happened.

## Inspiration

Every entry in this hackathon will build a memory store. Correct memory writes
are a solved problem: an idempotent upsert on a client-generated key handles it.

We wanted the harder problem. Once an agent takes real, non-idempotent action on
external infrastructure, correct memory is not sufficient. Its record of what it
did must match what actually happened in the world. Those are different claims,
and only the second one keeps you out of trouble at 3am.

## The thesis

> You cannot achieve exactly-once against a non-idempotent external API by retrying. Retry gets you at-least-once. You get exactly-once by pairing a durable intent log with an external verifier, and that intent log has to commit in the same transaction as the memory write, or the agent's history diverges from reality.

CockroachDB is the only memory layer where the action record, the world state,
and the vector embedding land in one serializable transaction. A separate vector
database cannot do this, because the memory write becomes a second transaction
that can fail alone.

We did not assert that. We built the competing architecture and tested it.

---

## Judging criteria

### Agentic Memory Design

CockroachDB is the whole memory layer, not a store bolted underneath one.

- **Episodic memory**: `episodes` with `VECTOR(1024)` Titan embeddings, recalled
  by cosine distance through a distributed vector index.
- **Semantic memory**: `playbooks` consolidated from recurring episodes. Steps are
  derived from `action_intents` that actually reached `COMMITTED`, and
  `derived_from` names the exact source episodes. Semantic memory is traceable to
  real behaviour rather than generated from model output.
- **Transactional memory**: `action_intents`, the durable record of intent to act,
  written *before* acting and resolved in the same transaction as the memory row.
- **Decay**: salience falls with age and row-level TTL removes faded episodes, so
  expiry is a database feature rather than an application cron. Episodes with
  committed actions are pinned forever. Memory decays; the record of what the
  agent did to the world does not.
- **Time travel**: "what did the agent believe at time T when it made that
  decision" is answered by `AS OF SYSTEM TIME` over MVCC, not by an
  application-maintained audit table that only records what someone remembered
  to log.

Retrieval respects the index's real constraint: only prefix columns are filtered
in SQL, and salience and recency are applied after retrieval, because a
non-prefix predicate silently disqualifies the index. That is asserted by
`EXPLAIN` tests at 2000 rows, and both documented traps are pinned as
regressions.

### Technical Implementation

- **Exactly-once protocol** with length-prefixed idempotency keys and canonical
  JSON, so argument ordering, numeric spelling, and map-versus-struct cannot
  change the key.
- **Compile-time safety**: `Verify` and `Effect` are methods on the action
  interface, so an action without a verification strategy cannot be registered
  and the program does not build. There is no path that skips it.
- **Three-valued verdicts.** `Unknown` exists because observing world state is
  not the same as attributing a change to your own call. It escalates to a human
  rather than guessing, and the registry refuses an `Unknown` carrying no reason.
- **Attributable verification.** The SQL username the agent creates is *derived
  from the idempotency key*, so an entry in the CockroachDB Cloud audit log
  containing that name proves this specific intent ran. `AUDIT_LOG_SOURCE_CLI`
  versus `_UI` even separates the agent from a human in the console.
- **Correct retry classification.** `40001` replays because CockroachDB already
  aborted the transaction. `40003`, `57P01`, and class `08` are ambiguous and
  route to the reconciler, never to a retry of the external call.
- **Reconciler** claims lapsed intents with `FOR UPDATE SKIP LOCKED` so several
  can run without serializing.

### Real-World Impact

On-call automation is where agents are being deployed right now, and it is
exactly where at-least-once is unacceptable. Scaling a cluster twice, taking two
backups, or provisioning duplicate access are not equivalent to doing it once.

The protocol here is not specific to CockroachDB operations. Any agent that
takes non-idempotent action, whether issuing refunds, sending messages,
provisioning infrastructure, or moving money, faces the same gap between "my call
failed" and "my call did not happen". The failure matrix and the three-valued
verdict transfer directly.

The memory layer compounds: the third incident of a class resolves from a
consolidated playbook whose steps trace back to actions that actually worked.

### Production Readiness

- Row-level security with `CREATE POLICY` for scope isolation, enforced in SQL
  rather than only in application code
- `sslmode=verify-full`, least-privilege SQL user, credentials from Secrets
  Manager and never from a file in the repo
- Managed MCP Server kept strictly read-only as the operator audit path, so
  inspection cannot become an unaudited second write path
- Structured JSON logs with request id, route, latency and row counts, and never
  memory content
- A schema `CHECK` prevents an intent reaching `COMMITTED` without evidence
- `crdb_internal` is **not** enabled by default. v26.2 restricts it and offers a
  session variable the database itself labels "not recommended", so we made it
  opt-in and the panel reports the view as restricted instead
- Benchmarks measured, not estimated. 16 concurrent agents on one logical action:
  1 committed, 15 deduplicated by phase 1, **0 lost updates**, on both a local
  node and CockroachDB Cloud

### Creativity and Originality

The original move is treating the agent's memory and its effect on the world as
one atomic fact rather than two things to keep in sync, and then proving the
distinction matters by building the alternative and watching it fail.

Most agent architectures reach for a vector database plus a relational database.
That is two transactions, and the second one can fail alone. `internal/control`
is a fair implementation of that design, including a test asserting it works
correctly when nothing fails, and it reproduces both failures the thesis
predicts.

---

## CockroachDB tools used

**Distributed Vector Indexing.** `CREATE VECTOR INDEX ... (scope_key, status,
embedding vector_cosine_ops)` over 1024-dimension Titan embeddings. The agent
recalls similar past incidents before deciding, and consolidates recurring ones
into playbooks whose embedding is the normalized centroid of their members. Index
use is proven by `EXPLAIN` assertions, not assumed.

**ccloud CLI.** The action surface. The agent provisions scoped diagnostic SQL
users during an incident, then verifies them against the organization audit log
via `ccloud audit list`, matching on the derived username. Live tests run against
a real cluster and create and delete real users.

**Cloud Managed MCP Server.** The read-only operator audit path. The agent writes
through the protocol; humans inspect through MCP. Keeping it read-only is
deliberate: a second, unaudited write path would defeat the intent log.

We did not use Agent Skills. A thin integration added to lengthen a list is worse
than an honest omission.

## AWS services used

**Amazon Bedrock.** Titan Text Embeddings V2 at 1024 dimensions generates every
embedding written to CockroachDB. The returned width is asserted at the call site
so a misconfigured model names itself rather than failing inside a commit.

**AWS Lambda.** Runs the agent and serves the observability panel behind a
Function URL. One binary, two runtimes: the HTTP handler is identical locally and
deployed, so the demo shows what actually runs.

**CloudWatch.** Structured JSON logs carrying request id, route, status and
latency, with memory content excluded by construction.

## What we learned

The database was not the hard part. Serializable transactions, the vector index,
row-level TTL, RLS and `SKIP LOCKED` all worked as documented. The friction was
in the edges between tools: pgx cannot bind CockroachDB's `VECTOR` type, row-level
TTL is accepted on a foreign-key-referenced table and then fails at run time with
`23503`, and the ccloud CLI cannot authenticate with a service account key, which
means a Lambda has to call the Cloud API directly instead. All of it is written
up honestly in `docs/feedback.md`.

The most valuable discovery was the audit log's `source` field, which is what
made honest attribution possible at all.

## What we would do next

Extend the verifier set to actions with weaker attribution, and use them to
demonstrate the `Unknown` path escalating to a human rather than guessing. That
is the case most agent frameworks get wrong, and it deserves its own demo.
