# MCP configuration sync

Bridge can keep the MCP servers of every agent in step. It is off by default:

```toml
# ~/.config/bridge/config.toml
[sync]
mcp_configs = true
mcp_exclude = ["bridge", "node_repl"]   # never synced; the default
```

## Sources

| Scope | Source | Claude Code | Codex |
| --- | --- | --- | --- |
| Global | `~/.config/bridge/mcp.toml` | `~/.claude.json` (`mcpServers`) | `~/.codex/config.toml` |
| Project | `<repo>/.agents/mcp.toml` | `<repo>/.mcp.json` | `<repo>/.codex/config.toml` (trusted projects only) |

```toml
[servers.db]
command = "dbtool"
args = ["serve"]
env = { DSN = "postgres://localhost/dev" }

[servers.docs]
url = "https://example.com/mcp"        # transport defaults to http; "sse" is also accepted
headers = { Authorization = "Bearer ..." }

# Fields only one agent understands go in a table named after the agent.
[servers.db.codex]
startup_timeout_sec = 30
```

Supported shared fields: `command`, `args`, `env`, `cwd`, `url`, `headers`, `transport`. Claude Code has no `cwd`, and Codex has no SSE transport; such servers are skipped for that agent with a reason.

## How entries are treated

Bridge edits only the servers it syncs; every other setting, and in Codex's TOML every comment, stays as it was. It records a fingerprint of each entry it writes, so it can tell its own entries apart:

| Situation | Result |
| --- | --- |
| Server in the source, missing in the agent | Added |
| Bridge's entry, source changed | Updated |
| Bridge's entry, edited by hand | Drift: kept; adopt it or overwrite from the source |
| The agent already had a different server of that name | Conflict: kept; adopt it or overwrite from the source |
| Server only in one agent | Offered for adoption into the source |
| Bridge's entry, removed from the source | Removed |

`bridge sync --adopt <agent>` copies that agent's version into the source and delivers it to the other agents in the same run; `bridge sync --force` makes the source win. `bridge uninstall` removes only the servers bridge added that nobody edited since. The source files themselves are yours and are never deleted.
