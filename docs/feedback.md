# Feedback on the CockroachDB AI tooling

Engineer to engineer, from six days of building against v26.2.5 and the Basic
tier. Written because the submission form asks and because most of it cost us
real time to discover.

Ordered by how much it would help the next person.

---

## 1. Row-level TTL is accepted on a table that a foreign key references, then fails at run time

This was the most expensive one.

```sql
ALTER TABLE episodes SET (ttl_expiration_expression = 'expires_at', ttl_job_cron = '@hourly');
```

Accepted without complaint. `action_intents` holds an FK to `episodes`. The TTL
job then fails at run time:

```
ERROR: delete on table "episodes" violates foreign key constraint
"action_intents_episode_id_fkey" on table "action_intents"
SQLSTATE: 23503
```

It fails on exactly the rows that matter most, the ones with committed actions,
and it fails in a background job rather than at the statement that caused it. A
DDL-time warning, or a note in the row-level TTL docs about FK-referenced tables,
would have saved a day.

Our fix: TTL keys off a nullable `expires_at`, and the transaction that binds an
action to an episode sets it to `NULL`, pinning that row forever. That turned out
to be a better design, but we arrived at it by debugging rather than by reading.

## 2. pgx cannot bind or scan `VECTOR`, and the failure is misleading

Passing a `[]float32` as a parameter:

```
ERROR: error in argument for $1: malformed vector literal:
Vector contents must start with "[" and end with "]" (SQLSTATE 22P02)
```

pgx encodes `[]float32` as a PostgreSQL array literal, `{1,2,3}`, and
CockroachDB wants `[1,2,3]`. Scanning is worse: it fails only at read time with
`invalid array, expected ':' got 44`, which does not point anywhere near the
real problem.

The fix is small once you know it, a type implementing `pgtype.TextValuer` and
`pgtype.TextScanner`, but nothing in the vector docs mentions pgx, and pgx is
the default Go driver. A documented snippet, or a codec shipped in a
`cockroachdb/pgx-vector` helper, would remove a sharp edge from the most likely
Go integration path.

## 3. The vector index traps are real, silent, and worth a louder warning

Both documented traps reproduce exactly as described, and both degrade to
`spans: FULL SCAN` with no warning, no notice, and no plan hint:

- Using `<->` against an index declared `vector_cosine_ops`.
- Adding any non-prefix predicate, for example `salience > 0.4`.

The second is the dangerous one, because filtering recall by salience or recency
is the obvious thing to write. We pinned both as regression tests. A `NOTICE`
when a query has an `ORDER BY <distance>` and a vector index exists on the table
but was not used would catch this at development time.

Related and also true: the optimizer will not choose a vector index on a small
table, so an `EXPLAIN` check against a nearly empty schema proves nothing. Our
tests seed 2000 rows and `ANALYZE` before asserting.

## 4. `AS OF SYSTEM TIME` rejects bind parameters entirely

```
ERROR: AS OF SYSTEM TIME: only constant expressions, with_min_timestamp,
with_max_staleness, or follower_read_timestamp are allowed (SQLSTATE XXUUU)
```

Reasonable. But the hint implies `with_min_timestamp($1)` would work, and it
does not either:

```
ERROR: expected timestamptz argument for min_timestamp (SQLSTATE 22023)
```

So the only route is string interpolation into the SQL, which is uncomfortable
advice to give and easy to do badly. We format a `time.Time` through a fixed
layout so no caller-controlled text can reach the query, but the natural first
attempt is to interpolate a user-supplied string. Accepting a placeholder inside
`with_min_timestamp` would remove that hazard.

## 5. `crdb_internal` is restricted in v26.2, which breaks a lot of published guidance

```
ERROR: Access to crdb_internal and system is restricted (SQLSTATE 42501)
HINT: These interfaces are unsupported in production. To proceed, set the session
variable allow_unsafe_internals = true (not recommended), or contact Cockroach
Labs for a supported alternative.
```

We wanted `crdb_internal.cluster_contention_events` for an observability panel,
which is exactly the use case a lot of existing blog posts and docs recommend.
The hint offers a session variable the message itself calls not recommended, and
"contact Cockroach Labs for a supported alternative" is not actionable inside a
hackathon.

We made it explicitly opt-in behind an environment variable rather than enabling
it by default, and the panel reports the view as restricted otherwise. A
supported, read-only contention view would be genuinely valuable; contention is
one of the first things anyone wants to see when they write a concurrent workload
on CockroachDB.

Smaller, same area: `cluster_contention_events` exposes `table_id` and
`index_id`, not names, so any human-readable panel needs a join against
`crdb_internal.tables`, which is behind the same restriction.

## 6. ccloud CLI cannot run non-interactively with a service account key

This shaped our deployment architecture, so it is worth flagging clearly.

The CLI authenticates through `ccloud auth login` and stores a session. We could
not find any way to drive it with a service account API key: the binary exposes
only `CCLOUD_PROFILE` and `CCLOUD_SERVER` as environment variables, and no
`--api-key` flag exists on the commands we needed.

That means **a Lambda cannot shell out to ccloud**. The deployed path has to call
the Cloud API over HTTPS with the key as a bearer token instead, which is a
different code path from the one used locally and in the demo. For a CLI marketed
as "Agent-Ready", running unattended in a serverless function seems like a
central use case. An `--api-key` flag or a `CCLOUD_API_KEY` variable would close
this.

## 7. ccloud output shapes are inconsistent between subcommands

Using `-o json` throughout, which is otherwise excellent and the right call:

- `cluster user list` returns a bare array: `[{"name":"viraj"}]`
- `cluster database list` returns an object: `{"databases":[...]}`
- `audit list` returns an array of entries

So a generic client needs to handle both shapes. Ours tries the array form, then
falls back to finding the first array-valued key in an object.

Also, `audit list`'s `payload` is a **JSON-encoded string**, not a nested object,
so it needs a second `json.Unmarshal`. That is easy to handle once known and
surprising the first time, especially since every other field is properly typed.

Minor: `cluster user list <cluster>` takes the cluster positionally while several
neighbouring commands use flags. We guessed `--cluster` first and got
`unknown flag`.

## 8. The audit log is genuinely excellent, and is the best thing we found

This deserves to be said as loudly as the complaints.

`ccloud audit list` gave us everything we needed to make verification honest:

```json
{
  "action": "AUDIT_LOG_ACTION_CREATE_SQL_USER",
  "cluster_id": "75981ff4-...",
  "created_at": "2026-08-12T04:23:16Z",
  "id": "382f4243-d2f0-4076-b2bf-9ad5731015d6",
  "payload": "{\"name\": \"anchor_422b6af752e68501\"}",
  "source": "AUDIT_LOG_SOURCE_CLI",
  "user_email": "..."
}
```

A unique operation id, the resource, the operation's own arguments, and a
`source` that distinguishes a CLI action from a human in the web console. That
last field is what let our agent tell its own action apart from an operator's,
which is the entire difficulty in verifying a non-idempotent call. We did not
expect to find it and the project is better because it exists.

Two small asks: a filter by action type or resource id, so verification does not
have to page through unrelated entries; and documentation of the retention window
and the ingestion delay, since we had to measure the lag ourselves to know how
long to poll.

## 9. Smaller things

- `array_fill()` does not exist, which is the natural way to build a test vector.
  We generate them with `(SELECT ARRAY_AGG(random()::FLOAT4) FROM generate_series(1,1024))::VECTOR(1024)`,
  which works well but is not obvious.
- `ALTER TABLE ... RESET (ttl_expiration_expression)` fails with
  `"ttl_expire_after" and/or "ttl_expiration_expression" must be set` while TTL
  is still enabled, so switching expressions needs a `SET` rather than a `RESET`
  first.
- `FOR UPDATE SKIP LOCKED` works well and made the multi-reconciler design
  straightforward. Worth advertising more; we nearly designed around its absence.

## What we would tell the next team

The database itself was not the hard part. Serializable transactions, the vector
index, row-level TTL, RLS, and `SKIP LOCKED` all worked as documented and are a
genuinely strong foundation for agent memory. The friction was concentrated in
the edges between tools: pgx and `VECTOR`, TTL and foreign keys, the CLI and
unattended execution. Those are the places to spend documentation effort.
