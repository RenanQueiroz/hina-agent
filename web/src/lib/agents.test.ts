import { describe, expect, it } from "vitest";
import {
  authTypeLabel,
  emptyLoginState,
  reduceLoginFrame,
  type LoginState,
} from "./agents";
import type { AgentLoginFrame } from "./api.gen";

function fold(frames: AgentLoginFrame[]): LoginState {
  return frames.reduce(reduceLoginFrame, emptyLoginState);
}

describe("reduceLoginFrame", () => {
  it("accumulates output lines", () => {
    const s = fold([
      { type: "output", text: "starting" },
      { type: "output", text: "still going" },
    ]);
    expect(s.lines).toEqual(["starting", "still going"]);
  });

  it("collects de-duplicated URL and code hints", () => {
    const s = fold([
      { type: "hint", hint: { kind: "url", value: "https://x/device" } },
      { type: "hint", hint: { kind: "code", value: "ABCD-1234" } },
      { type: "hint", hint: { kind: "code", value: "ABCD-1234" } },
    ]);
    expect(s.urls).toEqual(["https://x/device"]);
    expect(s.codes).toEqual(["ABCD-1234"]);
  });

  it("flags a paste prompt", () => {
    const s = fold([{ type: "hint", hint: { kind: "prompt", value: "Paste code:" } }]);
    expect(s.needsInput).toBe(true);
  });

  it("records a successful done and clears the input flag", () => {
    const s = fold([
      { type: "hint", hint: { kind: "prompt", value: "Paste:" } },
      { type: "done", ok: true },
    ]);
    expect(s.needsInput).toBe(false);
    expect(s.done).toEqual({ ok: true, error: "" });
  });

  it("records a failed done with its reason", () => {
    const s = fold([{ type: "done", ok: false, error: "login timed out" }]);
    expect(s.done).toEqual({ ok: false, error: "login timed out" });
  });

  it("ignores empty output text", () => {
    const s = fold([{ type: "output", text: "" }]);
    expect(s.lines).toEqual([]);
  });
});

describe("authTypeLabel", () => {
  it("maps known auth types", () => {
    expect(authTypeLabel("browser_state")).toBe("Browser login");
    expect(authTypeLabel("api_key")).toBe("API key");
    expect(authTypeLabel("local_llamacpp")).toBe("Local (llama.cpp)");
  });
  it("passes through an unknown type", () => {
    expect(authTypeLabel("weird")).toBe("weird");
  });
});
