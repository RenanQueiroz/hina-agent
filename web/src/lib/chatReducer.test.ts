import { describe, expect, it } from "vitest";
import { reduceEvent, type Msg } from "./chatReducer";
import type { Event as ServerEvent } from "./events.gen";
import {
  TypeAgentTextCompleted,
  TypeAgentTextDelta,
  TypeError,
  TypeUserTextSubmitted,
} from "./events.gen";

function ev(type: string, turnId: string, payload: Record<string, unknown>): ServerEvent {
  return {
    event_id: "evt",
    seq: 0,
    server_ts: "",
    source: "server",
    type,
    turn_id: turnId,
    payload,
  } as ServerEvent;
}

describe("reduceEvent", () => {
  it("adds a user message from the envelope turn_id", () => {
    const out = reduceEvent([], ev(TypeUserTextSubmitted, "t1", { text: "hi" }));
    expect(out).toEqual<Msg[]>([{ id: "t1", role: "user", text: "hi" }]);
  });

  it("streams assistant deltas then finalizes", () => {
    let s: Msg[] = [];
    s = reduceEvent(s, ev(TypeAgentTextDelta, "t2", { delta: "Hel" }));
    s = reduceEvent(s, ev(TypeAgentTextDelta, "t2", { delta: "lo" }));
    expect(s).toEqual<Msg[]>([{ id: "t2", role: "assistant", text: "Hello", streaming: true }]);
    s = reduceEvent(s, ev(TypeAgentTextCompleted, "t2", { text: "Hello", interrupted: false }));
    expect(s).toEqual<Msg[]>([
      { id: "t2", role: "assistant", text: "Hello", streaming: false, interrupted: false },
    ]);
  });

  it("is idempotent on replay (upsert by turn id)", () => {
    let s: Msg[] = [];
    const completed = ev(TypeAgentTextCompleted, "t3", { text: "done" });
    s = reduceEvent(s, completed);
    s = reduceEvent(s, completed); // replayed
    expect(s).toHaveLength(1);
    expect(s[0].text).toBe("done");
  });

  it("marks interrupted replies", () => {
    const s = reduceEvent([], ev(TypeAgentTextCompleted, "t4", { text: "part", interrupted: true }));
    expect(s[0].interrupted).toBe(true);
  });

  it("marks a turn errored on ErrorEvent, keeping partial text", () => {
    let s: Msg[] = [];
    s = reduceEvent(s, ev(TypeAgentTextDelta, "t5", { delta: "partial" }));
    s = reduceEvent(s, ev(TypeError, "t5", { error: "boom" }));
    expect(s[0].text).toBe("partial");
    expect(s[0].error).toBe(true);
    expect(s[0].streaming).toBe(false);
  });

  it("restores partial text on replay from the ErrorEvent payload", () => {
    // Replay has no live deltas, so the partial text must come from the event
    // payload — matching the canonical text the server feeds the model.
    const s = reduceEvent([], ev(TypeError, "t6", { error: "boom", text: "partial reply" }));
    expect(s[0].text).toBe("partial reply");
    expect(s[0].error).toBe(true);
    expect(s[0].streaming).toBe(false);
  });

  it("ignores events without a turn id", () => {
    const s = reduceEvent([], ev("SessionCreated", "", { title: "x" }));
    expect(s).toEqual([]);
  });
});
