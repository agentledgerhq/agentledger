#!/usr/bin/env node

import { startUserServer } from "./user-server.js";
import { startSupervisorServer } from "./supervisor-server.js";

const subcommand = process.argv[2];

if (subcommand === "user") {
  startUserServer().catch((err) => {
    console.error("Fatal:", err);
    process.exit(1);
  });
} else if (subcommand === "supervisor") {
  startSupervisorServer().catch((err) => {
    console.error("Fatal:", err);
    process.exit(1);
  });
} else {
  console.error("Usage: agentledger-mcp <user|supervisor>");
  console.error("");
  console.error("Subcommands:");
  console.error("  user        Start the user agent MCP server");
  console.error("  supervisor  Start the supervisor agent MCP server");
  console.error("");
  console.error("Environment variables:");
  console.error("  AGENTLEDGER_BASE_URL           API base URL (default: http://127.0.0.1:9119)");
  console.error("  AGENTLEDGER_USER_TOKEN         User token (alu_...) — required for 'user'");
  console.error("  AGENTLEDGER_SUPERVISOR_TOKEN   Supervisor token (als_...) — required for 'supervisor'");
  console.error("  AGENTLEDGER_SUPERVISOR_TIER    Supervisor tier 1-3 (default: 1)");
  process.exit(1);
}
