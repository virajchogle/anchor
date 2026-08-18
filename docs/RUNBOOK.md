# Morning runbook

Everything below has been run end to end against live infrastructure. Follow it
in order and the recording writes itself.

**Live demo:** https://5fokjzhq73.execute-api.us-east-1.amazonaws.com

## Before you start

```sh
cd ~/Desktop/crdbxaws
ccloud auth whoami   # should print your org
```

The demo commands read `~/.anchor/env` themselves, so there is nothing to source
and nothing to forget mid-recording. An explicitly set variable still wins if you
need to override one.

If `ccloud auth whoami` says you are logged out, run `ccloud auth login`.

Open the dashboard in a browser beside your terminal. It refreshes every 5
seconds on its own, so you never need to reload it on camera.

## Reset to a clean starting state

Only if the dashboard looks cluttered from testing.

```sh
go run ./cmd/demo -reset      # empty slate
```

**Record from empty.** Do not pre-seed. Watching the counters go from zero is the
"memory compounds" segment, and a dashboard that already has history gives that
away before you say it. The seeding run below IS step 2 of the recording.

Leave the browser on **Live run** for the whole recording. It animates on its own
while you talk, so you never have to touch it on camera.

That leaves 3 incidents, 1 playbook, and a green "Memory matches reality" banner.

---

## The recording, in order

### 1. The claim (0:00 to 0:25)

Sidebar → **How it works**. Read the highlighted paragraph verbatim. Then say the
plain-English version:

> Everyone else is building memory stores. Correct memory writes are a solved
> problem. This is about an agent that *acts*, and what happens when it loses
> track of what it did.

### 2. Memory compounds (0:25 to 0:55)

```sh
go run ./cmd/demo -incidents 3
```

Point at the terminal as it runs: incident 1 recalls **0** prior incidents,
incident 2 recalls **1**, incident 3 recalls **2**, then it writes its own
playbook.

Then sidebar → **Recall search**, type a symptom, hit enter. Show the similarity,
recency and salience bars. This is a real cosine query against the distributed
vector index, roughly 250ms round trip.

### 3. The core (0:55 to 1:55) — do not rush this

```sh
go run ./cmd/demo -crash
```

The agent creates a real SQL user on the real cluster, then dies before recording
it. **Stop talking and let the dashboard turn red.** It takes up to 5 seconds.

Say: the world has changed, and the agent's memory does not know. A system
without a durable intent log would do it a second time on restart.

```sh
go run ./cmd/demo -reconcile
```

It finds the orphan, checks the real CockroachDB audit log, and settles it
**without acting again**. Dashboard returns to green.

Then the control, which is the argument:

```sh
go test ./internal/control/ -run TestControl_DoubleActsOnCrash -v
```

> CONTROL DOUBLE-ACTED: the external world was scaled 2 times for one incident

And the quieter failure, with no crash at all:

```sh
go test ./internal/control/ -run TestControl_MemoryDivergesFromReality -v
```

### 4. Refusing to guess (1:55 to 2:20)

This is the strongest idea in the project and the part nobody else will have.

```sh
go run ./cmd/demo -escalate
go run ./cmd/demo -reconcile
```

Read the reconciler's reasoning off the screen. It refuses to record a deletion
it cannot prove it performed, because an absent user is equally consistent with
an operator having removed it. The intent stays PENDING and escalates.

Dashboard shows **Escalated to a human: 1**.

Say: most agent frameworks have two answers, done and not done. The third answer,
"I do not know, a human should look", is what keeps an agent from either doing
something twice or lying in its own history.

### 4b. Closing the loop (optional, 20 seconds)

An escalation is not a dead end. It is a work item for a person, and it is
answered **on the page**.

After `-escalate` and `-reconcile`, the Live run view shows the Verified stage in
amber with a decision box underneath it: a note field and two buttons, **I
confirmed it happened** and **I confirmed it did not**. Type a reason, click, and
the pipeline turns green while you are on camera. No terminal.

Say that it is recorded as `human_operator` evidence, never as machine
verification, so nobody later mistakes a person's assertion for an audit-log
fact. Also worth saying: the note is required, because an escalation closed
without a reason just moves the unexplained state somewhere else.

The same thing is available from the CLI if you prefer:

```sh
go run ./cmd/anchorctl escalations
go run ./cmd/anchorctl resolve <idem_key> applied "audit entry confirms it"
```

### 4c. Two more pages worth 15 seconds each

**Head to head.** The comparison is a recorded measurement, not a claim. Both
architectures, same external API, same crash point, real process kills:

```
crash between acting and recording    anchor 1 op    control 2 ops
memory write fails, no crash          impossible     world changed, memory empty
```

Re-measure any time with `go run ./cmd/compare`.

**Contention.** Set agents to 24 and click **Race them**. One square turns green,
twenty-three go blue, lost updates stays zero. Every agent proposed the identical
action, so all of them derived the same idempotency key and phase 1 let exactly
one through. It cleans up after itself and never touches the incident history.

### 5. Numbers (2:20 to 2:50)

Sidebar → **Time travel**, click "5 minutes ago", show what the agent believed
then. Read through MVCC, not an audit table.

Then `docs/RESULTS.md`: 16 concurrent agents on one logical action, 1 committed,
15 deduplicated, **0 lost updates**, on both a local node and CockroachDB Cloud.

Close on the compliance table in the README.

---

## Numbers you can quote, all measured

| Claim | Where it came from |
|---|---|
| 0 lost updates, 16 concurrent agents | `docs/RESULTS.md`, both environments |
| Recall similarity 0.846 for a repeat incident | live run, `cmd/demo` |
| Playbook confidence 0.743 from 3 episodes | live run |
| Live recall ~250ms | `/api/recall` against CockroachDB Cloud |
| Control double-acts: 2 operations | `TestControl_DoubleActsOnCrash`, and the Head to head page |
| 24 agents, 1 acted, 0 lost updates | the Contention page, live |
| A crash does not refund a customer twice | `TestPayments_CrashDoesNotRefundTwice` |

Do not quote anything that is not on this list.

## If something breaks on camera

- **Dashboard blank or erroring**: `aws logs tail /aws/lambda/anchor-panel --since 5m --region us-east-1`
- **ccloud says not logged in**: `ccloud auth login`
- **Demo says user already exists**: `go run ./cmd/demo -cleanup`
- **Everything looks stuck**: the reconciler is idempotent, just run
  `go run ./cmd/demo -reconcile` again.
- **An intent is stuck PENDING and reconciling will not clear it**: that is an
  escalation and it is meant to need you. `go run ./cmd/anchorctl escalations`
  shows why, then `anchorctl resolve <key> applied|failed "<note>"` closes it.

## After recording

```sh
go run ./cmd/demo -cleanup        # removes the SQL users the demo created
```

Submit: repo URL, the demo URL above, the video link, and the text from
`docs/DEVPOST.md`.

Rotate the AWS access key when judging is over. It was pasted in plaintext during
setup, so treat it as compromised once this is public.
