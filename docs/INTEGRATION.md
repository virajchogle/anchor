# How each component is integrated

The test we held ourselves to: **remove the component and describe what breaks.**
If the answer is "nothing important", it was initialized, not integrated.

## CockroachDB

### Distributed vector indexing

The agent's decisions depend on recall. Before acting it searches `episodes` and
`playbooks` by cosine distance to find what happened last time, and that result
is what makes the third incident of a class resolve faster than the first. On a
live run, recall similarity for a repeat incident measures 0.846.

The index did not just get used, it **shaped the architecture**. CockroachDB
permits only prefix columns in `WHERE` on a vector index, so a predicate on
salience or recency silently degrades the query to a full scan. That constraint
forced the retrieval design: SQL filters on `scope_key` and `status` only, we
over-fetch by 4x, and salience and recency are applied afterwards in Go. Both
failure modes are pinned as `EXPLAIN` regression tests at 2000 rows, because the
optimizer will not choose a vector index on a small table and asserting against
an empty schema proves nothing.

Consolidation writes a playbook whose embedding is the normalized centroid of its
member episodes, so recall compares a live symptom against the real shape of past
symptoms rather than against a generated summary.

**Remove it:** the agent has no recall, so nothing compounds and it is a script.

### ccloud CLI

Two distinct jobs, and the second is the important one.

It is the **action surface**: the agent provisions scoped diagnostic SQL users on
a real cluster during an incident, using the same interface a human on-call
engineer would use.

It is also the **verification source**. `ccloud audit list` is what makes the
exactly-once guarantee achievable at all. The agent derives the SQL username from
the idempotency key, so an audit entry whose payload contains that name is proof
that *this specific intent* ran, rather than an observation that the world happens
to look right. The audit log's `source` field even separates
`AUDIT_LOG_SOURCE_CLI` from `_UI`, distinguishing the agent's action from a human
working in the console.

**Remove it:** the agent cannot act, and more importantly it cannot verify, so
the protocol degrades to at-most-once plus a human on every action.

### The transaction that only CockroachDB provides

This is the part that cannot be substituted. Phase 3 commits the intent
resolution, the world-state mutation, and the memory row with its 1024-dimension
embedding in **one serializable transaction**. A separate vector database makes
the memory write a second transaction that can fail alone.

We did not assert that. `internal/control` is a fair implementation of the
standard architecture, including a test asserting it behaves correctly when
nothing fails. Against the identical incident, API, and crash point:

| Scenario | Anchor | Control |
|---|---|---|
| Crash between acting and recording | 1 external operation | **2 external operations** |
| Memory write fails, no crash | structurally impossible | world changed, memory empty |

Row-level TTL and row-level security are used the same way: TTL expires faded
memories as a database feature rather than an application cron, and an episode
with a committed action is pinned forever, so memory decays while the record of
what the agent did to production does not. `AS OF SYSTEM TIME` answers "what did
the agent believe at 3am" from MVCC rather than an audit table, and
`FOR UPDATE SKIP LOCKED` lets several reconcilers run without serializing.

## AWS

**Amazon Bedrock** generates every embedding written to CockroachDB. Titan Text
Embeddings V2 at 1024 dimensions, with the returned width asserted at the call
site so a misconfigured model names itself rather than failing later inside a
transaction. *Remove it: there is nothing to put in the vector index.*

**AWS Lambda** runs the agent and serves the operator console at the live demo
URL. One binary with two runtimes: the HTTP handler is identical locally and
deployed, so the demo shows what actually runs. *Remove it: there is no deployed
system and no demo.*

**API Gateway** fronts the Lambda. This was not a stylistic choice. Lambda
Function URLs are blocked by default on new AWS accounts; the resource policy and
auth type were correct and every request still returned 403, which direct
invocation proved was not a code problem. API Gateway uses the identical payload
format 2.0 event, so the handler needed no change. *Remove it: the demo returns
403 to everyone.*

**Secrets Manager** holds the database credential, which the Lambda reads at boot.
It is not decorative: the secret's `LastAccessedDate` confirms it is read on every
cold start. This keeps the connection string out of the Lambda environment, where
it would be visible in the console and in every `describe-function` response.
*Remove it: the credential moves into an environment variable anyone with console
read access can see.*

**CloudWatch** receives structured JSON logs carrying request id, route, status
and latency across 43 log streams. Memory content is excluded by construction
rather than by redaction, because incident narratives can contain customer detail.
*Remove it: the deployed system is unobservable.*

**IAM** enforces least privilege on the execution role: `GetSecretValue` scoped to
one secret ARN and `bedrock:InvokeModel` scoped to the specific model ARNs in use,
rather than a broad managed policy.

## The one-sentence version

CockroachDB is not storage underneath the agent; it is the component that makes
the agent's central guarantee possible, and we shipped the competing architecture
as a test to prove the difference is real rather than rhetorical.
