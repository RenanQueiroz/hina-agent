// Pure reducer that folds the conversation event stream into a renderable
// message list. Extracted from the Chat page so it is unit-testable. The
// turn id comes from the event envelope (e.turn_id); payloads carry only the
// text/delta. Upsert-by-turn-id makes replay idempotent.
import type { Event as ServerEvent } from "./events.gen";
import {
  TypeAgentTextCompleted,
  TypeAgentTextDelta,
  TypeUserTextSubmitted,
} from "./events.gen";

export interface Msg {
  id: string;
  role: string;
  text: string;
  streaming?: boolean;
  interrupted?: boolean;
}

function upsert(prev: Msg[], id: string, patch: Partial<Msg> & { role: string }): Msg[] {
  const i = prev.findIndex((m) => m.id === id);
  if (i === -1) return [...prev, { id, text: "", ...patch }];
  const next = prev.slice();
  next[i] = { ...next[i], ...patch };
  return next;
}

export function reduceEvent(prev: Msg[], e: ServerEvent): Msg[] {
  const turnId = e.turn_id ?? "";
  if (!turnId) return prev;
  const p = (e.payload ?? {}) as Record<string, unknown>;

  switch (e.type) {
    case TypeUserTextSubmitted:
      return upsert(prev, turnId, { role: "user", text: String(p.text ?? "") });
    case TypeAgentTextDelta: {
      const i = prev.findIndex((m) => m.id === turnId);
      const delta = String(p.delta ?? "");
      if (i === -1)
        return [...prev, { id: turnId, role: "assistant", text: delta, streaming: true }];
      const next = prev.slice();
      next[i] = { ...next[i], text: next[i].text + delta, streaming: true };
      return next;
    }
    case TypeAgentTextCompleted:
      return upsert(prev, turnId, {
        role: "assistant",
        text: String(p.text ?? ""),
        streaming: false,
        interrupted: Boolean(p.interrupted),
      });
    default:
      return prev;
  }
}
