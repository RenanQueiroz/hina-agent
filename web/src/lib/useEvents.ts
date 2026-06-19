import { useEffect } from "react";
import type { Event as ServerEvent } from "./events.gen";

// useConversationEvents opens an SSE stream for a conversation and calls onEvent
// for every event (replayed history + live). EventSource auto-reconnects and
// resumes via Last-Event-ID. The stream is same-origin, so the session cookie
// is sent automatically.
export function useConversationEvents(
  conversationId: string | null,
  onEvent: (e: ServerEvent) => void,
  onDisconnect?: () => void,
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
    // EventSource auto-reconnects (resuming via Last-Event-ID). We still notify
    // on disconnect so the UI can stop showing an in-progress draft as live: a
    // healthy turn's terminal event re-arrives on reconnect; a turn whose
    // finalization failed (server force-closes the stream) does not, so the
    // draft stays stopped instead of pulsing forever.
    es.onerror = () => onDisconnect?.();
    return () => es.close();
  }, [conversationId, onEvent, onDisconnect]);
}
