// Pure helpers for the Sandbox settings page, factored out so the form logic is
// unit-tested under the node-env vitest (no DOM needed).
import type { Event as ServerEvent } from "./events.gen";
import { TypeToolCallCompleted, TypeToolCallRequested } from "./events.gen";

// PendingApproval is a tool call awaiting the user's approve/deny decision.
export interface PendingApproval {
  callId: string;
  tool: string;
  summary: string;
}

// reduceApproval folds a tool-call event into the pending-approval list: a
// ToolCallRequested that needs approval is added (deduped by call id), and a
// ToolCallCompleted clears it (approved, denied, or finished). Pure, so the chat
// page's approval-card state is unit-tested without a DOM.
export function reduceApproval(
  prev: PendingApproval[],
  e: ServerEvent,
): PendingApproval[] {
  const p = (e.payload ?? {}) as Record<string, unknown>;
  const callId = typeof p.call_id === "string" ? p.call_id : "";
  if (e.type === TypeToolCallRequested && p.needs_approval && callId) {
    if (prev.some((a) => a.callId === callId)) return prev;
    return [
      ...prev,
      {
        callId,
        tool: typeof p.tool === "string" ? p.tool : "",
        summary: typeof p.summary === "string" ? p.summary : "",
      },
    ];
  }
  if (e.type === TypeToolCallCompleted && callId) {
    return prev.filter((a) => a.callId !== callId);
  }
  return prev;
}

// toggleTool adds or removes a tool from the allow-list, preserving order.
export function toggleTool(allowed: string[], tool: string): string[] {
  return allowed.includes(tool)
    ? allowed.filter((t) => t !== tool)
    : [...allowed, tool];
}

// isValidEnvName reports whether name is a valid environment-variable name
// (mirrors the server's validation so the UI can flag a bad grant early).
export function isValidEnvName(name: string): boolean {
  return /^[A-Za-z_][A-Za-z0-9_]*$/.test(name);
}

// humanTool maps a tool id to a friendly label for the checkbox list.
export function humanTool(tool: string): string {
  switch (tool) {
    case "shell":
      return "Shell (run a command)";
    case "fs_read":
      return "Read a file";
    case "fs_write":
      return "Write a file";
    case "http_fetch":
      return "Fetch a URL";
    default: {
      // Callable-agent run tools: "agent.codex.run" -> "Run the codex agent".
      const m = /^agent\.(.+)\.run$/.exec(tool);
      if (m) return `Run the ${m[1]} agent`;
      return tool;
    }
  }
}
