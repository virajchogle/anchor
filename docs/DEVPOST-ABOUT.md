**An on-call agent that can't lie to itself.**
**Its memory and its actions commit in the same transaction, or neither happens.**

## Inspiration

Every agent-memory framework we looked at treats memory as storage. Write things
down, embed them, search them later. That is a solved problem, and an idempotent
upsert on a client-generated key handles it.

We wanted the problem underneath it. The moment an agent stops answering
questions and starts *doing* things, correct memory is no longer sufficient. Its
record of what it did has to match what actually happened in the world.

Consider an agent that scales a database cluster. It issues the call and the
connection drops. It has no idea whether the scale went through.

- Assume it failed and retry, and the cluster scales twice.
- Assume it worked and it might be wrong, and the agent's memory now contains a
  fact that is false. Every later decision inherits that lie.

There is no third option available to a system that only has retries, and this
is not a corner case. It is what happens every time a process is killed, a
deployment rolls, or a network blips at the wrong moment.

The claim we built the project around:

> You cannot achieve exactly-once against a non-idempotent external API by
> retrying. Retry gets you at-least-once. You get exactly-once by pairing a
> durable intent log with an external verifier, and that intent log has to commit
> in the same transaction as the memory write, or the agent's history diverges
> from reality.

## What it does

**Anchor** is an autonomous on-call agent for CockroachDB Cloud. It diagnoses
incidents, takes real remediation actions through the `ccloud` CLI, and
accumulates institutional memory so the third incident of a class resolves faster
than the first.

Its actions are genuinely consequential and genuinely non-idempotent: creating a
scoped diagnostic SQL user twice is an error, not a no-op. That is the point.
An agent whose actions are safe to repeat does not need any of this.

## How we built it

**A three-phase protocol.** Phase 1 writes a durable intent *before* anything
external happens, so a crash leaves evidence that something may be in flight. Its
key is a length-prefixed hash, which prevents `("ab","c")` colliding with
`("a","bc")`:

$$
\text{idem\_key} = \mathrm{SHA256}\Big(\ell(\text{episode\_id}) \,\|\, \ell(\text{action\_type}) \,\|\, \ell(\mathrm{canonical\_json}(\text{args}))\Big)
$$

Canonicalization means key ordering, numeric spelling ($5$, $5.0$, $5\mathrm{e}0$)
and map-versus-struct cannot change the key, so two agents proposing the same
logical action always derive the same one.

Phase 2 makes the external call. Phase 3 commits the intent resolution, the world
state and the memory row with its 1024-dimension embedding in **one serializable
transaction**. That single transaction is the entire argument.

**A reconciler that never re-executes.** It claims lapsed intents with
`FOR UPDATE SKIP LOCKED`, asks the external system what actually happened, and
records the answer.

**Attribution, not observation.** This was the hardest idea to get right. A
verifier that reads "the cluster has 5 nodes" cannot tell our action from an
operator's. So the SQL username Anchor creates is *derived from the idempotency
key*. An entry in CockroachDB Cloud's audit log containing that name is proof
that **this** intent ran. The audit log even distinguishes
`AUDIT_LOG_SOURCE_CLI` from `_UI`, separating the agent from a human in the
console.

**Three-valued verdicts.** `Applied`, `NotApplied`, and `Unknown`. The third one
matters most: where evidence cannot establish authorship, Anchor refuses to guess
and escalates to a person, because guessing "done" fabricates history and
guessing "not done" repeats a destructive action.

**Memory that behaves like memory.** Recall combines similarity, recency and
salience, with recency as exponential decay on a half-life $t_{1/2}$:

$$
r = e^{-\ln 2 \cdot \Delta t / t_{1/2}}, \qquad
\text{score} = 0.6\,\text{sim} + 0.2\,r + 0.2\,\text{sal}
$$

Recurring episodes consolidate into playbooks whose steps are derived from
intents that actually committed, never generated. Salience decays and row-level
TTL reaps faded memories, but an episode with a committed action is pinned
forever: memory decays, the record of what the agent did to production does not.
`AS OF SYSTEM TIME` answers "what did the agent believe at 3am" from MVCC rather
than an audit table.

**Stack.** Go throughout. CockroachDB Cloud for all memory, with a distributed
vector index. Amazon Bedrock (Titan V2) for embeddings, AWS Lambda serving the
agent and console, Secrets Manager for credentials, CloudWatch for structured
logs.

## Challenges we ran into

**Row-level TTL silently fights foreign keys.** `ALTER TABLE ... SET
(ttl_expiration_expression = ...)` is accepted without complaint, then the TTL
job fails at run time with `23503` because `action_intents` references
`episodes`, and it fails on exactly the rows that matter most. We keyed TTL off a
nullable column that phase 3 sets to `NULL`, which turned a bug into the better
design.

**pgx cannot bind CockroachDB's `VECTOR` type.** A `[]float32` is encoded as a
PostgreSQL array literal `{1,2,3}` and rejected; reads fail later with an opaque
array-parse error. We wrote a codec implementing `pgtype.TextValuer` and
`TextScanner`.

**The vector index degrades silently.** Using `<->` against a `vector_cosine_ops`
index, or adding any non-prefix predicate like `salience > 0.4`, drops to a full
scan with no warning. Both are now pinned as `EXPLAIN` regression tests at 2000
rows, since the optimizer will not choose a vector index on a small table anyway.

**`crdb_internal` is restricted in v26.2.** The contention view a lot of guidance
recommends now requires a session variable the database itself labels "not
recommended". We made it explicitly opt-in rather than enabling it silently.

**The ccloud CLI cannot authenticate with a service account key.** It exposes
only `CCLOUD_PROFILE` and `CCLOUD_SERVER`, which means a Lambda cannot shell out
to it. That reshaped the deployment.

**Lambda Function URLs are blocked by default on new AWS accounts.** The resource
policy, the auth type and the function were all correct and every request still
returned 403. Direct invocation returned 200, which ruled out a code problem, and
API Gateway solved it with an identical payload format.

**Our own tests lied to us once.** The decay test passed while doing nothing: the
pinned episode was too fresh for the guard to be exercised. We only caught it by
deleting the guard and noticing the test still passed. Every load-bearing
assertion is now mutation-tested.

## What we learned

The database was not the hard part. Serializable transactions, the vector index,
row-level TTL, row-level security and `SKIP LOCKED` all worked as documented. The
friction lived in the seams between tools, and we wrote all of it up honestly in
`docs/feedback.md`.

The deeper lesson is that **exactly-once is a property of a protocol and a
verifier together, never of the protocol alone.** Where an API leaves no
attributable trace, the honest outcome degrades to at-most-once plus a human, and
saying so is better than pretending otherwise. Building the delete action taught
us this the hard way: we wrote a verifier that matched an audit entry by
username, then realised the username was not derived from our key, so it proved a
deletion happened and not that *ours* did. We fixed it to return `Unknown`.

## Accomplishments we're proud of

We did not assert the thesis, we tested it. `internal/control` is a fair
implementation of the standard architecture, including a test asserting it works
correctly when nothing fails. Against the identical incident, API and crash
point:

| Scenario | Anchor | Control |
|---|---|---|
| Crash between acting and recording | **1** external operation | **2** external operations |
| Memory write fails, no crash | structurally impossible | world changed, memory empty |

Other measured results: 24 concurrent agents on one logical action produced 1
action and **0 lost updates** in 355 ms against CockroachDB Cloud. Recall
similarity for a repeat incident is 0.846. 35 tests pass, including a chaos test
that kills a real operating-system process with `os.Exit(9)`.

And it generalises. `internal/payments` implements refunds against the same
interface with no changes to the coordinator, reconciler or key derivation. The
same crash, and the customer is not paid twice.

## What's next

Extending the verifier set to more actions with weak attribution, so the
`Unknown` path gets exercised across a wider surface. Every agent framework that
takes real actions needs this and none of them have it yet.
