import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import { Card, Spinner } from "../components/ui";

// Admin shell skeleton. Runtime/backend status, per-backend logs, and full user
// management fill in across later phases; this proves RequireAdmin + the
// admin-owned LLM policy surface.
export function AdminPage() {
  const users = useQuery({ queryKey: ["admin", "users"], queryFn: api.adminUsers });
  const llm = useQuery({ queryKey: ["admin", "llm"], queryFn: api.adminLLM });

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-6">
      <div>
        <h1 className="text-lg font-semibold">Admin</h1>
        <p className="text-sm text-zinc-500">
          Backend policy, runtime health, and user management (expands across later phases).
        </p>
      </div>

      <Card className="p-4">
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
          Active text backend
        </h2>
        {llm.isLoading ? (
          <Spinner />
        ) : (
          <dl className="grid grid-cols-[120px_1fr] gap-y-1 text-sm">
            <dt className="text-zinc-500">Provider</dt>
            <dd>{llm.data?.provider}</dd>
            <dt className="text-zinc-500">Model</dt>
            <dd>{llm.data?.model || <span className="text-zinc-400">—</span>}</dd>
            <dt className="text-zinc-500">Base URL</dt>
            <dd>{llm.data?.base_url || <span className="text-zinc-400">default</span>}</dd>
          </dl>
        )}
        <p className="mt-3 text-xs text-zinc-400">
          Users never choose STT/LLM/TTS — backend selection is admin-owned (config-driven for now).
        </p>
      </Card>

      <Card className="p-4">
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">Users</h2>
        {users.isLoading ? (
          <Spinner />
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="text-zinc-500">
              <tr>
                <th className="py-1 font-medium">Username</th>
                <th className="py-1 font-medium">Role</th>
                <th className="py-1 font-medium">Status</th>
                <th className="py-1 font-medium">Created</th>
              </tr>
            </thead>
            <tbody>
              {users.data?.map((u) => (
                <tr key={u.id} className="border-t border-zinc-100 dark:border-zinc-800">
                  <td className="py-1.5">{u.username}</td>
                  <td className="py-1.5">{u.role}</td>
                  <td className="py-1.5">{u.status}</td>
                  <td className="py-1.5 text-zinc-500">
                    {new Date(u.created_at).toLocaleDateString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      <LogsPanel />
    </div>
  );
}

function LogsPanel() {
  const [lines, setLines] = useState<string[]>([]);
  const boxRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const es = new EventSource("/api/v1/admin/logs");
    es.onmessage = (m) => setLines((prev) => [...prev, m.data].slice(-300));
    return () => es.close();
  }, []);
  useEffect(() => {
    boxRef.current?.scrollTo(0, boxRef.current.scrollHeight);
  }, [lines]);

  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Server logs
      </h2>
      <div
        ref={boxRef}
        className="h-64 overflow-auto rounded-md bg-zinc-950 p-3 font-mono text-xs leading-relaxed text-zinc-200"
      >
        {lines.length === 0 ? (
          <span className="text-zinc-500">Waiting for logs…</span>
        ) : (
          lines.map((l, i) => (
            <div key={i} className="whitespace-pre-wrap break-all">
              {l}
            </div>
          ))
        )}
      </div>
    </Card>
  );
}
