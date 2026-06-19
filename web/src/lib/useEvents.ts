import { useEffect } from "react";
import type { Event as ServerEvent } from "./events.gen";

// useConversationEvents opens an SSE stream for a conversation and calls onEvent
// for every event (replayed history + live). EventSource auto-reconnects and
// resumes via Last-Event-ID. The stream is same-origin, so the session cookie
// is sent automatically.
export function useConversationEvents(
  conversationId: string | null,
  onEvent: (e: ServerEvent) => void,
) {
  useEffect(() => {
    if (!conversationId) return;
    const es = new EventSource(
      `/api/v1/conversations/${conversationId}/events?since=0`,
    );
    es.onmessage = (m) => {
      try {
        onEvent(JSON.parse(m.data) as ServerEvent);
      } catch {
        /* ignore malformed frame */
      }
    };
    // onerror: EventSource reconnects on its own; nothing to do.
    return () => es.close();
  }, [conversationId, onEvent]);
}
