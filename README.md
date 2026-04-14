# AgentLedger

<p align="center">
  <a href="https://github.com/agentledgerhq/agentledger/actions"><img src="https://img.shields.io/github/actions/workflow/status/agentledgerhq/agentledger/release.yaml?label=build&style=flat-square" alt="Build"></a>
  <a href="https://github.com/agentledgerhq/agentledger/releases"><img src="https://img.shields.io/github/v/release/agentledgerhq/agentledger?style=flat-square&label=release" alt="Release"></a>
  <a href="https://www.npmjs.com/package/agentledger-mcp"><img src="https://img.shields.io/npm/v/agentledger-mcp?style=flat-square&label=npm" alt="npm"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue?style=flat-square" alt="License"></a>
</p>

> The financial control layer between AI agents and payment methods.

AgentLedger is a standalone service that securely vaults a payment card and gates access to it behind per-agent budgets, dual-token authorization, and an optional human-in-the-loop approval flow.

It is distributed as a single, zero-dependency Go binary and uses an embedded SQLite database. Included is an MCP (Model Context Protocol) server to seamlessly drop into Claude Desktop, Cursor, and other agent platforms.

---

| ~11 MB | <50 ms | <1 ms | ~10 MB | 0 |
|:---:|:---:|:---:|:---:|:---:|
| on disk | cold start | per request | RAM at runtime | runtime deps |

> Full ledger, vault, approval workflow, and API surface in under 2,500 lines of Go. Single static binary — no Python, no Node, no Docker.

---

## Core Features

- **No runtime dependencies**: Static Go binary with embedded SQLite — no C compiler, no system libraries.
- **Strict Budget Controls**: Cumulative or monthly caps mapped to free-form `agent_id` strings.
- **Two-Tier Authentication**: Distinct tokens for spending agents (`alu_`) and supervisor agents (`als_`).
- **Human-in-the-loop (Optional)**: Require manual approval before an agent sees card details.
- **Secure Vaulting**: In-memory AES-256-GCM encryption with `memguard` enclave protection.
- **MCP Native**: Official servers included to plug directly into LLM clients.

## Installation

### 1. Download Binary
Get the latest binary for your platform from [Releases](https://github.com/agentledgerhq/agentledger/releases).

```bash
# macOS/Linux
mv agentledger-linux-amd64 agentledger
chmod +x agentledger
./agentledger --help
```

```powershell
# Windows (PowerShell)
Rename-Item agentledger-windows-amd64.exe agentledger.exe
.\agentledger.exe --help
```

### 2. Build from Source
Requires [Go 1.25+](https://golang.org/doc/install). No C compiler needed.
```bash
git clone https://github.com/agentledgerhq/agentledger
cd agentledger/apps/local
go build -o agentledger ./cmd/agentledger
```

## Quick Start

```bash
# 1. Initialize DB and generate tokens (sets up .env)
agentledger init

# 2. Vault your credit card
agentledger vault add
# (No real card? Use --test-card to load a Stripe test card instead)
# agentledger vault add --test-card

# 3. Provision an agent's budget (e.g., $50.00 limit — amounts are in cents)
agentledger budget set researcher-agent 5000

# 4. Start the REST API
agentledger serve
```

*By default, the server listens on `http://127.0.0.1:9119`.*

## Setting up the MCP Server

AgentLedger includes an MCP (Model Context Protocol) integration exposing REST API endpoints as LLM tools.

Configure your MCP client (e.g., `claude_desktop_config.json`) with the two servers:

```json
{
  "mcpServers": {
    "agentledger-user": {
      "command": "npx",
      "args": ["agentledger-mcp", "user"],
      "env": {
        "AGENTLEDGER_USER_TOKEN": "alu_your-token"
      }
    },
    "agentledger-supervisor": {
      "command": "npx",
      "args": ["agentledger-mcp", "supervisor"],
      "env": {
        "AGENTLEDGER_SUPERVISOR_TOKEN": "als_your-token"
      }
    }
  }
}
```

The supervisor server exposes different tools depending on the tier set at `agentledger init`: tier 1 (default) handles transaction approvals; tier 2 adds budget and credit management; tier 3 adds vault updates.

<details>
<summary>Building from source instead of npx</summary>

```bash
cd apps/mcp && npm install && npm run build
```

Then replace `"command": "npx"` / `"args": ["agentledger-mcp", ...]` with:
```json
"command": "node",
"args": ["/absolute/path/to/apps/mcp/dist/index.js", "user"]
```
</details>

## REST API & Webhooks

When not using MCP, you can manually interact with the API:

```bash
# Request payment (Agent)
curl -X POST http://127.0.0.1:9119/authorize \
  -H "Authorization: Bearer alu_..." \
  -H "Content-Type: application/json" \
  -d '{"amount": 2500, "merchant": "AWS", "purpose": "EC2", "requesting_agent_id": "researcher-agent"}'
```

To receive real-time updates on approvals and denials, set `TRANSACTION_CALLBACK_URL` in your `.env`. AgentLedger will emit HMAC-SHA256 signed webhooks for every transaction outcome.

## Documentation

Full technical documentation, ledger semantics, and configuration guides are available at [agentledger.tech](https://agentledger.tech).

For LLMs, point them directly to our single-file documentation dump:
```
https://agentledger.tech/llms-full.txt
```

## License

AGPL-3.0 License. See [LICENSE](./LICENSE) for details.
