# agentledger-mcp

MCP (Model Context Protocol) servers for AgentLedger. Provides two servers matching the two-token role model:

- **`user`** — tools for spending agents (`request_payment`, `check_budget`, `get_status`, `get_transactions`)
- **`supervisor`** — tools for overseer agents (`list_pending_transactions`, `approve_transaction`, `deny_transaction`, `get_transactions`, and tier-gated `set_budget`, `credit_budget`, `update_vault`)

Both servers are thin proxies: they receive MCP tool calls over stdio and forward them as HTTP requests to the AgentLedger REST API.

## Prerequisites

- Node.js >= 18
- A running AgentLedger instance (`agentledger serve`)

## Setup

```bash
cd apps/mcp
npm install
npm run build
```

## Usage

```bash
# Start the user agent MCP server
AGENTLEDGER_USER_TOKEN=alu_... node dist/index.js user

# Start the supervisor agent MCP server
AGENTLEDGER_SUPERVISOR_TOKEN=als_... node dist/index.js supervisor

# Supervisor with tier 2 (adds set_budget, credit_budget)
AGENTLEDGER_SUPERVISOR_TOKEN=als_... AGENTLEDGER_SUPERVISOR_TIER=2 node dist/index.js supervisor
```

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `AGENTLEDGER_BASE_URL` | No | `http://127.0.0.1:9119` | AgentLedger API base URL |
| `AGENTLEDGER_USER_TOKEN` | Yes (user) | — | User token (`alu_` prefix) |
| `AGENTLEDGER_SUPERVISOR_TOKEN` | Yes (supervisor) | — | Supervisor token (`als_` prefix) |
| `AGENTLEDGER_SUPERVISOR_TIER` | No | `1` | Supervisor tier (1-3), controls which tools are exposed |

## MCP Client Configuration

### Claude Desktop

Add to `claude_desktop_config.json`:

**After npm publish** (once the package is on npm):

```json
{
  "mcpServers": {
    "agentledger-user": {
      "command": "npx",
      "args": ["agentledger-mcp", "user"],
      "env": {
        "AGENTLEDGER_USER_TOKEN": "alu_your-token-here"
      }
    },
    "agentledger-supervisor": {
      "command": "npx",
      "args": ["agentledger-mcp", "supervisor"],
      "env": {
        "AGENTLEDGER_SUPERVISOR_TOKEN": "als_your-token-here",
        "AGENTLEDGER_SUPERVISOR_TIER": "2"
      }
    }
  }
}
```

**Before npm publish** (local development — use absolute path to the built entry point):

```json
{
  "mcpServers": {
    "agentledger-user": {
      "command": "node",
      "args": ["/absolute/path/to/apps/mcp/dist/index.js", "user"],
      "env": {
        "AGENTLEDGER_USER_TOKEN": "alu_your-token-here"
      }
    }
  }
}
```

### Cursor / Windsurf

Same structure — add the server entries to your MCP configuration file.

## Supervisor Tiers

| Tier | Tools |
|---|---|
| 1 (default) | `list_pending_transactions`, `approve_transaction`, `deny_transaction`, `get_transactions` |
| 2 | + `set_budget`, `credit_budget` |
| 3 | + `update_vault` |

## Architecture

```
MCP Client (Claude, Cursor, etc.)
    ↕ stdio (JSON-RPC)
agentledger-mcp (user or supervisor)
    ↕ HTTP (REST)
agentledger serve (localhost:9119)
```

The MCP servers never store state — they are stateless proxies. All state lives in the AgentLedger server's SQLite database.
