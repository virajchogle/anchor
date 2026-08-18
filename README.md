# Anchor

**Transactional memory for agents that act.**

> You cannot achieve exactly-once against a non-idempotent external API by retrying. Retry gets you at-least-once. You get exactly-once by pairing a durable intent log with an external verifier, and that intent log has to commit in the same transaction as the memory write, or the agent's history diverges from reality.

Anchor is an autonomous on-call agent for CockroachDB Cloud clusters. It diagnoses
incidents, takes real remediation actions through the ccloud CLI, and accumulates
institutional memory so the third incident of a class resolves faster than the first.

Most agent-memory systems solve a solved problem: writing things down correctly.
An idempotent upsert on a client-generated key handles that. Anchor solves the
harder one. When an agent takes consequential, non-idempotent action on external
infrastructure, correct memory is not sufficient. **The agent's record of what it
did must match what actually happened in the world.**

CockroachDB is the only memory layer where the action record, the world state,
and the vector embedding land in one serializable transaction. A separate vector
database cannot do this, because the memory write becomes a second transaction
that can fail alone. That is not an assertion here. It is
[a test](internal/control/control_test.go) that reproduces the failure.

---

## Compliance

Two or more CockroachDB tools and one or more AWS services are required. Every
row points at the file and function that satisfies it.

### CockroachDB tools (3 of 4 used)

| Tool | Where | What the agent actually does with it |
|---|---|---|
| **Distributed Vector Indexing** | [`db/schema.sql`](db/schema.sql), [`internal/memory/recall.go`](internal/memory/recall.go) | `CREATE VECTOR INDEX ... (scope_key, status, embedding vector_cosine_ops)` over 1024-dimension Titan embeddings. Recall filters on prefix columns only and applies salience and recency in Go, because a non-prefix predicate silently disqualifies the index. Index use is proven by `EXPLAIN` assertions at 2000 rows in [`recall_test.go`](internal/memory/recall_test.go), and both documented traps are pinned as regressions. |
| **ccloud CLI** | [`internal/ccloud/client.go`](internal/ccloud/client.go), [`internal/ccloud/action_sqluser.go`](internal/ccloud/action_sqluser.go) | The agent's action surface. It provisions scoped diagnostic SQL users during an incident and then verifies them against the organization audit log (`ccloud audit list`), matching on a username derived from the idempotency key. Live tests in [`action_sqluser_live_test.go`](internal/ccloud/action_sqluser_live_test.go) run against a real cluster. |
| **Cloud Managed MCP Server** | [`docs/mcp.md`](docs/mcp.md) | Read-only operator audit path. Kept strictly read-only: the agent writes through the protocol, humans inspect through MCP. |
| Agent Skills Repo | not used | Deliberately skipped. A thin integration added to lengthen a list is worse than an honest omission. |

### AWS services

| Service | Where | What it does |
|---|---|---|
| **Amazon Bedrock** | [`internal/bedrock/embed.go`](internal/bedrock/embed.go) | Titan Text Embeddings V2 at 1024 dimensions generates every embedding written to CockroachDB. Returned width is asserted at the call site so a misconfigured model names itself instead of failing inside a commit. |
| **AWS Lambda** | [`cmd/anchord/main.go`](cmd/anchord/main.go) | Runs the agent and serves the observability panel behind a Function URL. One binary, two runtimes; the HTTP handler is identical locally and deployed. |
| **CloudWatch** | [`internal/panel/panel.go`](internal/panel/panel.go) | Structured JSON logs carrying request id, route, status, and latency. Memory content is never logged, by construction rather than by redaction. |
| **Secrets Manager** | [`cmd/anchord/main.go`](cmd/anchord/main.go), [`deploy/deploy.sh`](deploy/deploy.sh) | The database credential is read from Secrets Manager at boot, so it never appears in a Lambda environment variable, the console, or a `describe-function` response. |

---

## The core: exactly-once action protocol

Full write-up with the crash-point failure matrix in [`docs/protocol.md`](docs/protocol.md).

**Phase 1, Intend.** Write the durable intent *before* touching the world.
`idem_key = sha256(episode_id, action_type, canonical_json(args))`, hashed with
length-prefixed fields so `("ab","c")` cannot collide with `("a","bc")`.
`INSERT ... ON CONFLICT DO NOTHING` is the deduplication primitive: exactly one
caller gets a row, everyone else branches on the existing state.

**Phase 2, Execute.** The external call. It may change the world and still return
an error. That possibility is the entire reason phase 3 cannot be trusted to run.

**Phase 3, Commit.** Intent resolution, world state, and the memory row with its
embedding, in **one serializable transaction**.

```sql
BEGIN;
  UPDATE action_intents SET state='COMMITTED', external_ref=$2, outcome=$3, resolved_at=now() ...;
  UPDATE managed_clusters SET desired_nodes=COALESCE($4, desired_nodes), version=version+1 ...;
  UPDATE episodes SET narrative=$5, outcome=$6, embedding=$7, status='resolved', expires_at=NULL ...;
COMMIT;
```

**The reconciler.** Claims lapsed intents with `FOR UPDATE SKIP LOCKED`, asks the
external system what actually happened, and records the answer. It never
re-executes an action.

### Verdicts are three-valued, and that is the point

`Applied`, `NotApplied`, and **`Unknown`**. Observing world state is not the same
as attributing a change to your own call. A verifier that reads "the cluster has
5 nodes" cannot tell our action from an operator's. Returning `Applied` there
manufactures false history; returning `NotApplied` causes a double action. So
`Unknown` escalates to a human instead of guessing, and the registry refuses an
`Unknown` verdict that carries no explanation.

Anchor's ccloud verifier avoids that trap by making evidence attributable: the
SQL username is *derived from the idempotency key*, so an audit entry containing
it proves **this** intent ran.

### Retry policy

- **`40001` serialization failure**: replay. CockroachDB already aborted the
  transaction, so nothing took effect. Replaying phase 3 re-runs SQL only, never
  the external call. Exponential backoff with full jitter so colliding writers do
  not retry in lockstep.
- **`40003`, `57P01`, and class `08`**: **never replay.** The commit may have
  landed and we simply never heard. These route to the reconciler, which
  establishes ground truth first. This is precisely the case where a naive retry
  double-acts.

---

## Proof, not assertion

| Claim | Test |
|---|---|
| Crash between action and commit does not double-act | [`TestChaos_CrashBetweenExecuteAndCommit`](internal/protocol/chaos_test.go) kills a **real OS process** with `os.Exit(9)`, then asserts the reconciler commits memory matching reality and the world still shows exactly one operation |
| The standard architecture *does* double-act | [`TestControl_DoubleActsOnCrash`](internal/control/control_test.go): same incident, same API, same crash point, **2 external operations** |
| Separate vector store diverges from reality | [`TestControl_MemoryDivergesFromReality`](internal/control/control_test.go): no crash at all, world changed, memory empty, nothing to roll back |
| The control is not a strawman | [`TestControl_SucceedsWhenNothingFails`](internal/control/control_test.go) |
| Verification is attributable, not inferred | [`TestLive_VerifyIsAttributable`](internal/ccloud/action_sqluser_live_test.go): a second intent verifies `NotApplied` even while another intent's user exists on the same cluster |
| The vector index is actually used | [`TestRecall_UsesVectorIndex`](internal/memory/recall_test.go) asserts on `EXPLAIN` output at 2000 rows |
| Decay never un-pins a committed episode | [`TestDecay_NeverUnpinsCommittedEpisodes`](internal/memory/lifecycle_test.go) |

Load-bearing assertions were mutation-tested: the guard was removed and the test
was confirmed to fail. The decay test initially passed *vacuously* and was fixed
only because the mutation did not fail it.

Measured numbers are in [`docs/RESULTS.md`](docs/RESULTS.md), generated by
`cmd/bench`. Nothing in that file is written by hand.

---

## Setup from a clean machine

Requires Go 1.24+, and Docker only for the local database option.

```sh
git clone https://github.com/virajchogle/anchor && cd anchor

# 1. A database. Either a local node:
docker run -d --name crdb -p 26257:26257 cockroachdb/cockroach:latest start-single-node --insecure
docker exec crdb ./cockroach sql --insecure -e "CREATE DATABASE anchor"
export ANCHOR_DB_URL='postgresql://root@localhost:26257/anchor?sslmode=disable'

#    ...or CockroachDB Cloud (Basic tier is sufficient and free):
#    ccloud auth login && ccloud cluster list
#    export ANCHOR_DB_URL='postgresql://USER:PASS@HOST:26257/anchor?sslmode=verify-full'

# 2. Schema
go run ./cmd/anchorctl migrate db/schema.sql

# 3. Tests. These are integration tests against the real database.
go test ./...

# 4. The panel
go run ./cmd/anchord     # http://localhost:8080
```

Optional, and each is independent:

```sh
# Real ccloud actions against a live organization (creates and deletes SQL users)
ccloud auth login
ANCHOR_CCLOUD_LIVE=1 CCLOUD_CLUSTER_ID=<id> go test ./internal/ccloud/ -v

# Benchmarks
go run ./cmd/bench -db "$ANCHOR_DB_URL" -out docs/RESULTS.md

# The chaos demo: kill a real process mid-action and watch recovery
go test ./internal/protocol/ -run TestChaos -v

# The control, which double-acts under the same failure
go test ./internal/control/ -v
```

Credentials are read from the environment. Nothing is committed; `.gitignore`
blocks `*.env`. Deployed, they come from AWS Secrets Manager.

## Deploy to AWS

One idempotent command. It creates the IAM role, stores the connection string in
Secrets Manager, builds an arm64 Lambda bundle, deploys it behind a Function URL,
and prints the demo URL.

```sh
source ~/.anchor/env
./deploy/deploy.sh
```

The execution role is least-privilege by construction: `GetSecretValue` on
exactly one secret ARN, and `bedrock:InvokeModel` on exactly the two model ARNs
in use. Re-running updates in place rather than duplicating resources.

```sh
aws logs tail /aws/lambda/anchor-panel --follow    # structured JSON logs
```

## Security

- Row-level security with `CREATE POLICY` for scope isolation, enforced in SQL rather than only in application code
- `sslmode=verify-full`
- Least-privilege SQL user for the agent, DML only
- Managed MCP Server kept strictly read-only as the operator audit path
- Structured logs carry request id, route, latency, and row counts, never memory content
- `crdb_internal` access is **not** enabled by default; see [`docs/feedback.md`](docs/feedback.md)

## Documentation

- [`docs/protocol.md`](docs/protocol.md) the protocol and its crash-point failure matrix
- [`docs/architecture.md`](docs/architecture.md) diagram and component walkthrough
- [`docs/RESULTS.md`](docs/RESULTS.md) generated benchmark output
- [`docs/feedback.md`](docs/feedback.md) engineer-to-engineer notes on the CockroachDB AI tooling
- [`docs/mcp.md`](docs/mcp.md) Managed MCP Server as the read-only operator path

## License

MIT. All code was written for this hackathon.
