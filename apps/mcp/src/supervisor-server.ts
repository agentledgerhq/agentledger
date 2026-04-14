import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { createApiClient } from "./api-client.js";

export async function startSupervisorServer() {
  const token = process.env.AGENTLEDGER_SUPERVISOR_TOKEN;
  if (!token) {
    console.error(
      "Error: AGENTLEDGER_SUPERVISOR_TOKEN environment variable is required",
    );
    process.exit(1);
  }

  const tier = parseInt(process.env.AGENTLEDGER_SUPERVISOR_TIER || "1", 10);
  if (tier < 1 || tier > 3) {
    console.error("Error: AGENTLEDGER_SUPERVISOR_TIER must be 1, 2, or 3");
    process.exit(1);
  }

  const api = createApiClient(token);

  const server = new McpServer({
    name: "agentledger-supervisor",
    version: "1.0.0",
  });

  // ════════════════════════════════════════════
  // Tier 1 tools (always registered)
  // ════════════════════════════════════════════

  server.tool(
    "list_pending_transactions",
    "List all payment requests currently awaiting supervisor approval. Returns transaction IDs, amounts, merchants, purposes, and expiry times. Use this to discover what needs to be approved or denied.",
    {},
    async () => {
      try {
        const result = await api.get("/pending");
        return {
          content: [
            { type: "text" as const, text: JSON.stringify(result, null, 2) },
          ],
        };
      } catch (e) {
        return {
          content: [
            {
              type: "text" as const,
              text: `Error: ${e instanceof Error ? e.message : String(e)}`,
            },
          ],
          isError: true,
        };
      }
    },
  );

  server.tool(
    "approve_transaction",
    "Approve a pending payment request. Once approved, the transaction transitions to 'spent' and the reserved budget is consumed. Card details are NOT returned in this response — the user agent retrieves them by polling get_status with a matching agent_id. Only call this after reviewing the transaction via list_pending_transactions.",
    {
      transaction_id: z
        .string()
        .describe(
          "The unique transaction ID to approve (from list_pending_transactions).",
        ),
    },
    async ({ transaction_id }) => {
      try {
        const result = await api.post("/approve", { transaction_id });
        return {
          content: [
            { type: "text" as const, text: JSON.stringify(result, null, 2) },
          ],
        };
      } catch (e) {
        return {
          content: [
            {
              type: "text" as const,
              text: `Error: ${e instanceof Error ? e.message : String(e)}`,
            },
          ],
          isError: true,
        };
      }
    },
  );

  server.tool(
    "deny_transaction",
    "Deny a pending payment request. The reserved budget is fully refunded to the agent. The requesting agent will receive a 'denied' status when it next polls for its transaction status.",
    {
      transaction_id: z
        .string()
        .describe(
          "The unique transaction ID to deny (from list_pending_transactions).",
        ),
    },
    async ({ transaction_id }) => {
      try {
        const result = await api.post("/deny", { transaction_id });
        return {
          content: [
            { type: "text" as const, text: JSON.stringify(result, null, 2) },
          ],
        };
      } catch (e) {
        return {
          content: [
            {
              type: "text" as const,
              text: `Error: ${e instanceof Error ? e.message : String(e)}`,
            },
          ],
          isError: true,
        };
      }
    },
  );

  server.tool(
    "get_transactions",
    "Retrieve the transaction history for a specific agent. Returns all recorded transactions including their status, amounts, merchants, purposes, and timestamps.",
    {
      agent_id: z
        .string()
        .describe(
          "The identifier of the agent whose transaction history to retrieve.",
        ),
    },
    async ({ agent_id }) => {
      try {
        const result = await api.get(
          `/transactions/${encodeURIComponent(agent_id)}`,
        );
        return {
          content: [
            { type: "text" as const, text: JSON.stringify(result, null, 2) },
          ],
        };
      } catch (e) {
        return {
          content: [
            {
              type: "text" as const,
              text: `Error: ${e instanceof Error ? e.message : String(e)}`,
            },
          ],
          isError: true,
        };
      }
    },
  );

  // ════════════════════════════════════════════
  // Tier 2 tools (budget management)
  // ════════════════════════════════════════════

  if (tier >= 2) {
    server.tool(
      "set_budget",
      "Create or update the spending budget for a user agent. Period can be 'cumulative' (never resets) or 'monthly' (resets on the 1st of each month). Defaults to 'cumulative'.",
      {
        agent_id: z
          .string()
          .describe("The identifier of the agent whose budget is being set."),
        limit: z
          .number()
          .int()
          .positive()
          .describe("The budget limit in cents (e.g. 10000 for $100.00)."),
        period: z
          .enum(["cumulative", "monthly"])
          .optional()
          .describe(
            "Budget period: 'cumulative' (never resets) or 'monthly' (resets on the 1st of each month). Defaults to 'cumulative'.",
          ),
      },
      async ({ agent_id, limit, period }) => {
        try {
          const body: Record<string, unknown> = { limit };
          if (period) body.period = period;
          const result = await api.post(
            `/budget/${encodeURIComponent(agent_id)}`,
            body,
          );
          return {
            content: [
              { type: "text" as const, text: JSON.stringify(result, null, 2) },
            ],
          };
        } catch (e) {
          return {
            content: [
              {
                type: "text" as const,
                text: `Error: ${e instanceof Error ? e.message : String(e)}`,
              },
            ],
            isError: true,
          };
        }
      },
    );

    server.tool(
      "credit_budget",
      "Restore previously consumed budget for an agent. Use this after a payment bounces, a refund is received, or a transaction needs to be reversed. A reason is mandatory for audit trail purposes.",
      {
        agent_id: z.string().describe("The identifier of the agent to credit."),
        amount: z
          .number()
          .int()
          .positive()
          .describe(
            "Amount to restore in cents (e.g. 2500 for $25.00). Must be positive.",
          ),
        reason: z
          .string()
          .describe(
            "Human-readable reason for the credit (e.g. 'Payment bounced', 'Merchant refund received'). Required for audit trail.",
          ),
      },
      async ({ agent_id, amount, reason }) => {
        try {
          const result = await api.post("/credit", {
            agent_id,
            amount,
            reason,
          });
          return {
            content: [
              { type: "text" as const, text: JSON.stringify(result, null, 2) },
            ],
          };
        } catch (e) {
          return {
            content: [
              {
                type: "text" as const,
                text: `Error: ${e instanceof Error ? e.message : String(e)}`,
              },
            ],
            isError: true,
          };
        }
      },
    );
  }

  // ════════════════════════════════════════════
  // Tier 3 tools (vault management)
  // ════════════════════════════════════════════

  if (tier >= 3) {
    server.tool(
      "update_vault",
      "Replace the vaulted payment card with new credentials. REQUIRES SUPERVISOR TIER 3. This is a high-privilege operation: after this call, all future approved transactions will use the new card. Only available when the human operator explicitly enabled Tier 3 during 'agentledger init'. Use with extreme caution.",
      {
        holder_name: z.string().describe("Full name of the cardholder."),
        card_number: z
          .string()
          .describe("Card number (digits only, no spaces or dashes)."),
        expiry_date: z
          .string()
          .describe("Card expiry date in MM/YY format."),
        cvv: z.string().describe("Card CVV / security code."),
        country: z
          .string()
          .describe("Two-letter country code (e.g. US, GB, DE)."),
        zip_code: z.string().describe("Billing ZIP or postal code."),
      },
      async ({ holder_name, card_number, expiry_date, cvv, country, zip_code }) => {
        try {
          const result = await api.post("/vault/update", {
            holder_name,
            card_number,
            expiry_date,
            cvv,
            country,
            zip_code,
          });
          return {
            content: [
              { type: "text" as const, text: JSON.stringify(result, null, 2) },
            ],
          };
        } catch (e) {
          return {
            content: [
              {
                type: "text" as const,
                text: `Error: ${e instanceof Error ? e.message : String(e)}`,
              },
            ],
            isError: true,
          };
        }
      },
    );
  }

  const transport = new StdioServerTransport();
  await server.connect(transport);
  console.error(
    `agentledger-supervisor MCP server running on stdio (tier ${tier})`,
  );
}
