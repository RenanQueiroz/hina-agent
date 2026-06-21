import { describe, expect, it } from "vitest";
import {
  humanTool,
  isValidEnvName,
  reduceApproval,
  toggleTool,
  type PendingApproval,
} from "./sandbox";
import type { Event as ServerEvent } from "./events.gen";

function ev(type: string, payload: Record<string, unknown>): ServerEvent {
  return { event_id: "e", seq: 0, server_ts: "", source: "sandbox", type, payload } as ServerEvent;
}

describe("toggleTool", () => {
  it("adds a tool not present", () => {
    expect(toggleTool(["shell"], "fs_read")).toEqual(["shell", "fs_read"]);
  });
  it("removes a tool present", () => {
    expect(toggleTool(["shell", "fs_read"], "shell")).toEqual(["fs_read"]);
  });
});

describe("isValidEnvName", () => {
  it("accepts valid names", () => {
    expect(isValidEnvName("API_KEY")).toBe(true);
    expect(isValidEnvName("_x1")).toBe(true);
  });
  it("rejects invalid names", () => {
    expect(isValidEnvName("1BAD")).toBe(false);
    expect(isValidEnvName("has space")).toBe(false);
    expect(isValidEnvName("")).toBe(false);
  });
});

describe("humanTool", () => {
  it("labels known tools and passes through unknown", () => {
    expect(humanTool("shell")).toContain("Shell");
    expect(humanTool("mystery")).toBe("mystery");
  });
});

describe("reduceApproval", () => {
  it("adds a request needing approval and dedupes", () => {
    let s: PendingApproval[] = [];
    s = reduceApproval(s, ev("ToolCallRequested", { call_id: "c1", tool: "shell", summary: "shell: ls", needs_approval: true }));
    expect(s).toEqual<PendingApproval[]>([{ callId: "c1", tool: "shell", summary: "shell: ls" }]);
    s = reduceApproval(s, ev("ToolCallRequested", { call_id: "c1", tool: "shell", needs_approval: true }));
    expect(s.length).toBe(1);
  });
  it("ignores auto-approved requests", () => {
    const s = reduceApproval([], ev("ToolCallRequested", { call_id: "c2", needs_approval: false }));
    expect(s).toEqual([]);
  });
  it("clears on completion", () => {
    const start: PendingApproval[] = [{ callId: "c1", tool: "shell", summary: "" }];
    expect(reduceApproval(start, ev("ToolCallCompleted", { call_id: "c1" }))).toEqual([]);
  });
});
