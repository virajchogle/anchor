**An on-call agent that can't lie to itself.**
**Its memory and its actions commit in the same transaction, or neither happens.**

## Inspiration

Every agent-memory framework treats memory as storage: write it down, search it
later. That's solved.

The moment an agent *acts*, correct memory isn't enough. Its record has to match
what actually happened. An agent scales a cluster, the connection drops, and it
doesn't know whether it worked. Retry and it scales twice. Assume success and its
memory now holds a lie that every later decision inherits.

> You cannot achieve exactly-once against a non-idempotent external API by
> retrying. Retry gets you at-least-once. You get exactly-once by pairing a
> durable intent log with an external verifier, and that intent log has to commit
> in the same transaction as the memory write, or the agent's history diverges
> from reality.

## What it does

**Anchor** is an autonomous on-call agent for CockroachDB Cloud. It diagnoses
incidents, takes real actions through the `ccloud` CLI, and gets faster at each
recurring incident class. Its actions are deliberately non-idempotent: creating a
SQL user twice is an error, not a no-op.

## How we built it

Three phases. **Intend** writes a durable record *before* acting, keyed by a
length-prefixed hash so `("ab","c")` can't collide with `("a","bc")`:

$$\text{idem\_key} = \mathrm{SHA256}\big(\ell(\text{episode\_id}) \,\|\, \ell(\text{action\_type}) \,\|\, \ell(\mathrm{canonical\_json}(\text{args}))\big)$$

**Execute** makes the call. **Commit** writes the intent resolution, world state
and the 1024-dimension embedding in **one serializable transaction**. That
transaction is the whole argument.

Two ideas carry the rest. **Attribution over observation:** the SQL username is
derived from the idempotency key, so an entry in CockroachDB Cloud's audit log
containing it proves *this* intent ran, rather than that the world merely looks
right. And **three-valued verdicts:** applied, not applied, and `Unknown`. Where
evidence can't establish authorship, Anchor escalates to a human instead of
guessing, because guessing "done" fabricates history and guessing "not done"
repeats a destructive action.

Go, CockroachDB Cloud with a distributed vector index, Bedrock for embeddings,
Lambda serving the agent and console.

## Challenges

- **Row-level TTL silently fights foreign keys.** Accepted at DDL time, then
  fails at run time with `23503` on exactly the rows that matter. Fixing it
  produced a better design: memory decays, the record of what the agent did does
  not.
- **The vector index degrades silently.** Wrong operator or a non-prefix
  predicate drops to a full scan with no warning. Both are now `EXPLAIN`
  regression tests.
- **Our own test lied to us.** The decay guard was never exercised, so the test
  passed while doing nothing. We caught it by deleting the guard and noticing.
  Every load-bearing assertion is now mutation-tested.
- **pgx can't bind `VECTOR`**, `crdb_internal` is restricted in v26.2, the ccloud
  CLI can't authenticate with a service account key, and Lambda Function URLs are
  blocked by default on new AWS accounts. All written up in `docs/feedback.md`.

## What we learned

**Exactly-once is a property of a protocol and a verifier together, never the
protocol alone.** We learned it the hard way: our delete verifier matched an
audit entry by username, then we realised the username wasn't derived from our
key, so it proved *a* deletion happened, not that ours did. It now returns
`Unknown`.

## What we're proud of

We tested the thesis instead of asserting it. `internal/control` is a fair
implementation of the standard architecture. Same incident, same API, same crash:

| Scenario | Anchor | Control |
|---|---|---|
| Crash between acting and recording | **1** external operation | **2** external operations |
| Memory write fails, no crash | structurally impossible | world changed, memory empty |

24 concurrent agents on one action: 1 acted, **0 lost updates**, 355 ms. 35 tests
pass, including one that kills a real OS process mid-action. And it generalises:
`internal/payments` implements refunds against the same interface with no protocol
changes, and the same crash doesn't pay a customer twice.
