# The exactly-once action protocol

> You cannot achieve exactly-once against a non-idempotent external API by retrying. Retry gets you at-least-once. You get exactly-once by pairing a durable intent log with an external verifier, and that intent log has to commit in the same transaction as the memory write, or the agent's history diverges from reality.

This document is the engineering write-up of that claim: the mechanism, the
failure matrix at every crash point, and the places where the guarantee is
weaker than the headline and we say so.

## Why retry is not enough

Retrying a failed call assumes you know it failed. Against a real API you often
do not. The connection drops after the request is sent; the server commits and
the response is lost; the process dies between acting and recording. In each
case the caller sees an error and the world has already changed.

Retrying there produces a second action. That is at-least-once. It is the
correct default for idempotent operations and actively harmful for the rest.
Scaling a cluster twice, taking two backups, creating a user that already
exists: these are not equivalent to doing it once.

The only way to close the gap is to establish, from outside your own process,
whether the action happened. That requires two things: a durable record written
**before** acting, so recovery knows to look; and a verifier that can answer
**about your specific call**, not about the general state of the world.

## The three phases

### Phase 1, Intend

```sql
INSERT INTO action_intents (idem_key, episode_id, agent_id, action_type, args,
                            state, lease_owner, lease_expires, attempts)
VALUES ($1, $2, $3, $4, $5, 'PENDING', $3, now() + $6::INTERVAL, 1)
ON CONFLICT (idem_key) DO NOTHING
RETURNING idem_key;
```

`idem_key = sha256(episode_id, action_type, canonical_json(args))`.

Two details that matter more than they look:

**Length-prefixed hashing.** Each field is written with an explicit 8-byte
length before its bytes. Without it, `("ab","c")` and `("a","bc")` hash
identically. See `TestIdemKey_ConcatenationIsUnambiguous`.

**Canonical JSON.** Key order, numeric spelling (`5`, `5.0`, `5e0`), and
map-versus-struct must not change the key, or two agents proposing the same
logical action derive different keys and both act. Non-ASCII object keys are
rejected rather than sorted under an ordering that disagrees with RFC 8785,
because failing loudly beats a duplicate action later.

If no row is returned, another actor owns the intent. Branch on its state:

| Existing state | Disposition | Action |
|---|---|---|
| `COMMITTED` | `AlreadyCommitted` | Return the recorded outcome. Do not act. |
| `PENDING`, live lease | `Busy` | Back off and re-read. |
| `PENDING`, lapsed lease | `Orphaned` | **Do not take over and execute.** Only the reconciler may resolve it. |
| `FAILED` | `Failed` | Surface to an operator. Never silently retry. |

The `Orphaned` case is the one that matters. The obvious move is to grab the
stale lease and run the action. That is exactly wrong: the dead process may have
already made the call.

### Phase 2, Execute

The external call. It runs outside any transaction and may leave the world
changed even when it returns an error.

The idempotency key is handed to the external system wherever the API accepts a
token, because that is what later makes verification attributable.

### Phase 3, Commit

```sql
BEGIN;
  UPDATE action_intents
     SET state='COMMITTED', external_ref=$2, outcome=$3, resolved_at=now(), lease_owner=NULL
   WHERE idem_key=$1 AND state='PENDING';
  UPDATE managed_clusters
     SET desired_nodes=COALESCE($4, desired_nodes), last_action=$5, version=version+1
   WHERE cluster_id=$6;
  UPDATE episodes
     SET narrative=$7, outcome=$8, embedding=$9, status='resolved', expires_at=NULL
   WHERE episode_id=$10;
COMMIT;
```

**This single transaction is the thesis.** If the memory write were a second
transaction, or lived in a separate vector database, it could fail alone and
leave the agent's history disagreeing with what it did. Reproduced in
`TestControl_MemoryDivergesFromReality`.

`WHERE ... AND state='PENDING'` means zero rows affected aborts the commit rather
than clobbering an outcome another actor already verified.

`expires_at=NULL` pins the episode against row-level TTL. Memory decays; the
record of what the agent did to the world does not.

## The reconciler

On startup and on a timer, claim lapsed intents:

```sql
WITH claimable AS (
  SELECT idem_key FROM action_intents
   WHERE state = 'PENDING' AND lease_expires < now()
   ORDER BY lease_expires LIMIT $3
   FOR UPDATE SKIP LOCKED
)
UPDATE action_intents ai SET lease_owner=$1, lease_expires=now()+$2::INTERVAL,
       attempts=attempts+1
  FROM claimable c WHERE ai.idem_key = c.idem_key
RETURNING ...;
```

`SKIP LOCKED` lets several reconcilers run without serializing behind one slow
verification. Verified available on v26.2.5.

For each claimed intent, dispatch to the verifier and record what it says. **The
reconciler never re-executes an action.** It only observes.

## Verdicts are three-valued

| Verdict | Meaning | Reconciler behaviour |
|---|---|---|
| `Applied` | Confirmed, attributably, that **this** action took effect | Commit phase 3 with recovered memory |
| `NotApplied` | Confirmed the action did not take effect | Mark `FAILED`, surface to an operator |
| `Unknown` | Could not establish ground truth | Leave `PENDING`, back off, escalate |

`Unknown` is the important one, and most designs omit it.

Observing world state is not the same as attributing a change to your own call.
A verifier that reads "the cluster has 5 nodes" cannot distinguish our scale
committing from an operator scaling by hand from a previous identical intent.
Returning `Applied` manufactures false history. Returning `NotApplied` causes a
double action. So `Unknown` refuses to guess, and the registry rejects an
`Unknown` verdict carrying no explanation, because an escalation without a
reason wastes the operator's time.

Anything that leaves evidence incomplete produces `Unknown`: a failed read, an
undecodable payload, or an audit window that returned its full limit and may
therefore be truncated.

### Making evidence attributable

Anchor's `create_sql_user` action avoids the state-observation trap. The SQL
username is **derived from the idempotency key**, so an audit entry containing
that name is proof that this specific intent ran.

```
audit log entry 382f4243-... records AUDIT_LOG_ACTION_CREATE_SQL_USER
for user "anchor_422b6af752e68501" on cluster 75981ff4-...
at 2026-08-12T04:23:16Z via AUDIT_LOG_SOURCE_CLI
```

`AUDIT_LOG_SOURCE_CLI` versus `_UI` even separates an agent action from a human
in the console. `TestLive_VerifyIsAttributable` asserts that a *different*
intent still verifies `NotApplied` while this user exists.

Not every action can do this. Where an API offers no attributable trace, the
honest outcome is `Unknown` and escalation, not a confident guess.

## Failure matrix

Crash points, left to right through one action.

| # | Crash point | World | Intent row | Memory | Recovery | Double-acts? |
|---|---|---|---|---|---|---|
| 1 | Before phase 1 | unchanged | none | none | Nothing to do | No |
| 2 | During phase 1 insert | unchanged | may exist `PENDING` | none | Reconciler verifies `NotApplied`, marks `FAILED` | No |
| 3 | After phase 1, before phase 2 | unchanged | `PENDING` | none | Verifier finds no evidence, `NotApplied` | No |
| 4 | **During phase 2** | **may have changed** | `PENDING` | none | Verifier asks the external system | **No** |
| 5 | **After phase 2, before phase 3** | **changed** | `PENDING`, no `external_ref` | none | Verifier finds the audit entry, commits recovered memory | **No** |
| 6 | During phase 3, ambiguous commit | changed | unknown | unknown | `AmbiguousCommitError`, routed to reconciler | No |
| 7 | During phase 3, `40001` | changed | unchanged, aborted | unchanged | Replay SQL only | No |
| 8 | After phase 3 commits | changed | `COMMITTED` | written | Retry gets `AlreadyCommitted` | No |

Rows 4 and 5 are the ones this design exists for.
`TestChaos_CrashBetweenExecuteAndCommit` reproduces row 5 by killing a real
process with `os.Exit(9)`. The control implementation double-acts on the same row.

## Retry policy

| SQLSTATE | Meaning | Response | Why |
|---|---|---|---|
| `40001` | serialization failure | **Replay** with exponential backoff plus full jitter | CockroachDB already aborted the transaction, so nothing took effect. Replaying phase 3 re-runs SQL only, never the external call. |
| `40003` | statement completion unknown | **Reconciler** | CockroachDB's explicit signal that it cannot tell us whether the statement committed. |
| `57P01` | admin shutdown | **Reconciler** | The node went away mid-transaction; the commit may or may not have replicated. |
| class `08` | connection exception | **Reconciler** | Connection dropped at a point we cannot reason about. |
| anything else | | Surface | |

Jitter is not decoration. Concurrent agents on one scope abort each other with
`40001`; without jitter they retry in lockstep and collide again on the same
schedule, converting one contention event into a sustained livelock.

## Where the guarantee is weaker than the headline

Stated plainly rather than buried:

1. **Exactly-once is a property of the pair**, protocol plus verifier. With an
   action whose API leaves no attributable trace, the protocol degrades to
   at-most-once plus escalation. It never degrades to double-acting, but it does
   require a human.
2. **The audit log is eventually consistent.** Verification polls. An intent
   verified in the gap falls back to the resource listing, which is still
   attributable because we chose the name.
3. **A truncated audit window yields `Unknown`, not `NotApplied`.** Correct, but
   it means a very busy organization escalates more.
4. **Phase 1 can leave an orphan** if the process dies mid-insert. Harmless: the
   verifier reports `NotApplied` and it is cleared.
