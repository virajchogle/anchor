# CockroachDB Cloud Managed MCP Server

Anchor uses the Managed MCP Server as the **read-only operator audit path**, and
nothing else.

## Why read-only, deliberately

The agent already has a write path, and it is a careful one: every mutation goes
through the three-phase protocol, gets a durable intent, and is verified against
external ground truth before it is recorded as done. Giving a second, unaudited
write path to the same data would defeat that. An operator inspecting the
cluster through MCP can read anything and change nothing.

That split is the design:

- **The agent writes** through `internal/protocol`, with an intent log and a verifier.
- **Humans read** through MCP, with the Cloud's own audit logging.

## Configuration

Endpoint: `https://cockroachlabs.cloud/mcp`

For Claude Code, Cursor, or VS Code:

```json
{
  "mcpServers": {
    "cockroachdb": {
      "url": "https://cockroachlabs.cloud/mcp",
      "headers": { "Authorization": "Bearer ${CCLOUD_API_KEY}" }
    }
  }
}
```

`CCLOUD_API_KEY` is a service account key from
**https://cockroachlabs.cloud/access → Service Accounts → API Keys**. Note that
creating the service account and creating the key are two separate steps in that
UI, which is easy to miss.

## What an operator asks it

The questions worth asking during and after an incident map onto the tables the
protocol maintains:

- Which intents are `PENDING` with `attempts` above 3? Those are the `Unknown`
  escalations that need a human, and they are the only thing the agent
  deliberately refuses to resolve on its own.
- For a given `idem_key`, what does `outcome` say the evidence was? Every
  committed intent cites the source that settled it.
- Which episodes have `expires_at IS NULL`? Those are pinned because an action
  committed against them, so the audit trail is intact.
- Does `managed_clusters.version` equal the number of committed intents for that
  cluster? A mismatch would be a lost update.

## Scope of use

The MCP server is one of three CockroachDB tools Anchor uses, alongside
distributed vector indexing and the ccloud CLI. It is not load-bearing for the
agent's own operation, and that is the point: it is the path a human uses to
check the agent's work.
