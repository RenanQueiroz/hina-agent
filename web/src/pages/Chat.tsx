import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Plus, Send, Square } from "lucide-react";
import { api } from "../lib/api";
import { useConversationEvents } from "../lib/useEvents";
import type { Event as ServerEvent } from "../lib/events.gen";
import { reduceEvent, type Msg } from "../lib/chatReducer";
import { Button, Spinner } from "../components/ui";

export function ChatPage() {
  const qc = useQueryClient();
  const conversations = useQuery({ queryKey: ["conversations"], queryFn: api.listConversations });
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState("");
  const abortRef = useRef<AbortController | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);

  // Auto-select the most recent conversation once loaded.
  useEffect(() => {
    if (selectedId == null && conversations.data && conversations.data.length > 0) {
      setSelectedId(conversations.data[0].id);
    }
  }, [conversations.data, selectedId]);

  // Reset the timeline when switching conversations; SSE replay rebuilds it.
  useEffect(() => {
    setMessages([]);
  }, [selectedId]);

  const onEvent = useCallback((e: ServerEvent) => {
    setMessages((prev) => reduceEvent(prev, e));
  }, []);
  useConversationEvents(selectedId, onEvent);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  const newConversation = useMutation({
    mutationFn: () => api.createConversation(""),
    onSuccess: (conv) => {
      qc.invalidateQueries({ queryKey: ["conversations"] });
      setSelectedId(conv.id);
    },
  });

  const send = useMutation({
    mutationFn: async (text: string) => {
      if (!selectedId) return;
      const ac = new AbortController();
      abortRef.current = ac;
      try {
        await api.postMessage(selectedId, text, ac.signal);
      } finally {
        abortRef.current = null;
      }
    },
  });

  const submit = () => {
    const text = input.trim();
    if (!text || !selectedId || send.isPending) return;
    setInput("");
    send.mutate(text);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  };

  return (
    <div className="flex h-full">
      {/* Sidebar: conversations */}
      <aside className="flex w-64 flex-col border-r border-zinc-200 dark:border-zinc-800">
        <div className="p-2">
          <Button className="w-full" onClick={() => newConversation.mutate()}>
            <Plus size={16} /> New chat
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
          {conversations.isLoading && <Spinner />}
          {conversations.data?.map((c) => (
            <button
              key={c.id}
              onClick={() => setSelectedId(c.id)}
              className={`mb-1 w-full truncate rounded-md px-3 py-2 text-left text-sm ${
                c.id === selectedId
                  ? "bg-indigo-50 text-indigo-700 dark:bg-indigo-950 dark:text-indigo-300"
                  : "text-zinc-700 hover:bg-zinc-100 dark:text-zinc-300 dark:hover:bg-zinc-800"
              }`}
            >
              {c.title || "Untitled chat"}
            </button>
          ))}
          {conversations.data?.length === 0 && (
            <p className="px-3 py-2 text-sm text-zinc-400">No chats yet.</p>
          )}
        </div>
      </aside>

      {/* Main: timeline + composer */}
      <section className="flex min-w-0 flex-1 flex-col">
        {!selectedId ? (
          <div className="grid flex-1 place-items-center text-zinc-400">
            Create a chat to get started.
          </div>
        ) : (
          <>
            <div className="min-h-0 flex-1 overflow-y-auto">
              <div className="mx-auto flex max-w-3xl flex-col gap-4 p-4">
                {messages.length === 0 && (
                  <p className="py-8 text-center text-sm text-zinc-400">
                    Send a message to start the conversation.
                  </p>
                )}
                {messages.map((m) => (
                  <MessageBubble key={m.id} msg={m} />
                ))}
                <div ref={bottomRef} />
              </div>
            </div>
            <div className="border-t border-zinc-200 p-3 dark:border-zinc-800">
              <div className="mx-auto flex max-w-3xl items-end gap-2">
                <textarea
                  value={input}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={onKeyDown}
                  rows={1}
                  placeholder="Message Hina…"
                  className="max-h-40 min-h-[44px] flex-1 resize-y rounded-md border border-zinc-300 bg-white px-3 py-2 text-sm text-zinc-900 outline-none focus:border-indigo-500 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-100"
                />
                {send.isPending ? (
                  <Button variant="danger" onClick={() => abortRef.current?.abort()}>
                    <Square size={16} /> Stop
                  </Button>
                ) : (
                  <Button onClick={submit} disabled={!input.trim()}>
                    <Send size={16} /> Send
                  </Button>
                )}
              </div>
            </div>
          </>
        )}
      </section>
    </div>
  );
}

function MessageBubble({ msg }: { msg: Msg }) {
  const isUser = msg.role === "user";
  return (
    <div className={`flex ${isUser ? "justify-end" : "justify-start"}`}>
      <div
        className={`group relative max-w-[80%] whitespace-pre-wrap rounded-2xl px-4 py-2 text-sm ${
          isUser
            ? "bg-indigo-600 text-white"
            : "bg-white text-zinc-900 ring-1 ring-zinc-200 dark:bg-zinc-900 dark:text-zinc-100 dark:ring-zinc-800"
        }`}
      >
        {msg.text}
        {msg.streaming && <span className="ml-0.5 animate-pulse">▋</span>}
        {msg.interrupted && (
          <span className="ml-2 text-xs italic opacity-70">[interrupted]</span>
        )}
        {msg.error && <span className="ml-2 text-xs italic text-red-400">[failed]</span>}
        {!isUser && msg.text && (
          <button
            onClick={() => navigator.clipboard?.writeText(msg.text)}
            className="absolute -right-2 -top-2 hidden rounded-md bg-zinc-100 p-1 text-zinc-500 group-hover:block dark:bg-zinc-800"
            aria-label="Copy"
          >
            <Copy size={12} />
          </button>
        )}
      </div>
    </div>
  );
}
