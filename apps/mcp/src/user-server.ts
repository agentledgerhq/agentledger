import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { createApiClient } from "./api-client.js";

export async function startUserServer() {
  const token = process.env.AGENTLEDGER_USER_TOKEN;
  if (!token) {
    console.error("Error: AGENTLEDGER_USER_TOKEN environment variable is required");
    process.exit(1);
  }

  const api = createApiClient(token);

  const server = new McpServer({
    name: "agentledger-user",
    version: "1.0.0",
  });

  // ── request_payment → POST /authorize ──

  server.tool(
    "request_payment",
    "Request payment authorization for a specific merchant and amount. In auto mode, if the request is under the agent's budget, funds are consumed and credit card details are returned immediately. In manual mode, the transaction enters 'pending' state and the agent must poll get_status to retrieve card details after supervisor approval. There is no confirmation step — authorize is final.",
    {
      amount: z
        .number()
        .int()
        .positive()
        .describe("Amount in cents (e.g. 2500 for $25.00). Must be greater than zero."),
      merchant: z.string().describe("The name of the merchant where the payment will be made."),
      purpose: z.string().describe("A brief explanation of why this payment is necessary."),
      requesting_agent_id: z
        .string()
        .describe("The identifier of the agent requesting the payment."),
      metadata: z
        .record(z.unknown())
        .optional()
        .describe("Optional arbitrary key-value pairs to attach to the transaction for tracking purposes."),
    },
    async ({ amount, merchant, purpose, requesting_agent_id, metadata }) => {
      try {
        const result = await api.post("/authorize", {
          amount,
          merchant,
          purpose,
          requesting_agent_id,
          metadata,
        });
        return {
          content: [{ type: "text" as const, text: JSON.stringify(result, null, 2) }],
        };
      } catch (e) {
        return {
          content: [
            { type: "text" as const, text: `Error: ${e instanceof Error ? e.message : String(e)}` },
          ],
          isError: true,
        };
      }
    },
  );

  // ── check_budget → GET /budget/{agent} ──

  server.tool(
    "check_budget",
    "Check the available budget for a specific agent. Use this before requesting a payment to ensure funds are available.",
    {
      agent_id: z.string().describe("The identifier of the agent."),
    },
    async ({ agent_id }) => {
      try {
        const result = await api.get(`/budget/${encodeURIComponent(agent_id)}`);
        return {
          content: [{ type: "text" as const, text: JSON.stringify(result, null, 2) }],
        };
      } catch (e) {
        return {
          content: [
            { type: "text" as const, text: `Error: ${e instanceof Error ? e.message : String(e)}` },
          ],
          isError: true,
        };
      }
    },
  );

  // ── get_status → GET /status/{tx_id}?agent_id=... ──

  server.tool(
    "get_status",
    "Check the current status of a transaction. Returns status ('pending', 'spent', or 'denied'), amount, merchant, purpose, and expiry time for pending transactions. Card details are included only when the status is 'spent' and agent_id matches the transaction's owning agent. Use this to poll for approval results and retrieve card details after a request_payment call in manual approval mode.",
    {
      transaction_id: z
        .string()
        .describe("The unique transaction ID returned by request_payment."),
      agent_id: z
        .string()
        .describe(
          "Your agent identifier. Must match the requesting_agent_id used in the original request_payment call to receive card details.",
        ),
    },
    async ({ transaction_id, agent_id }) => {
      try {
        const result = await api.get(
          `/status/${encodeURIComponent(transaction_id)}?agent_id=${encodeURIComponent(agent_id)}`,
        );
        return {
          content: [{ type: "text" as const, text: JSON.stringify(result, null, 2) }],
        };
      } catch (e) {
        return {
          content: [
            { type: "text" as const, text: `Error: ${e instanceof Error ? e.message : String(e)}` },
          ],
          isError: true,
        };
      }
    },
  );

  // ── get_transactions → GET /transactions/{agent} ──

  server.tool(
    "get_transactions",
    "Retrieve the transaction history for a specific agent. Returns all recorded transactions including their status, amounts, merchants, purposes, and timestamps.",
    {
      agent_id: z
        .string()
        .describe("The identifier of the agent whose transaction history to retrieve."),
    },
    async ({ agent_id }) => {
      try {
        const result = await api.get(
          `/transactions/${encodeURIComponent(agent_id)}`,
        );
        return {
          content: [{ type: "text" as const, text: JSON.stringify(result, null, 2) }],
        };
      } catch (e) {
        return {
          content: [
            { type: "text" as const, text: `Error: ${e instanceof Error ? e.message : String(e)}` },
          ],
          isError: true,
        };
      }
    },
  );

  const transport = new StdioServerTransport();
  await server.connect(transport);
  console.error("agentledger-user MCP server running on stdio");
}
