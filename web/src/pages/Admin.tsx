import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  createColumnHelper,
  flexRender,
  getCoreRowModel,
  useReactTable,
} from "@tanstack/react-table";
import type { AdminUser, RTCSessionStats } from "../lib/api.gen";
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
        {users.isLoading ? <Spinner /> : <UsersTable users={users.data ?? []} />}
      </Card>

      <LiveSessionsPanel />
      <LogsPanel />
    </div>
  );
}

// LiveSessionsPanel polls the WebRTC stats so an admin can watch active voice
// sessions and their loss/jitter/RTT in near real time (Phase 3 instrumentation).
function LiveSessionsPanel() {
  const rtc = useQuery({
    queryKey: ["admin", "rtc"],
    queryFn: api.adminRTC,
    refetchInterval: 2000,
  });
  const sessions = rtc.data?.sessions ?? [];
  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Live voice sessions
      </h2>
      {sessions.length === 0 ? (
        <p className="text-sm text-zinc-400">No active sessions.</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="text-zinc-500">
              <tr>
                {["Session", "Mode", "Pkts in", "Loss", "Jitter", "RTT", "Played", "Drops"].map((h) => (
                  <th key={h} className="py-1 pr-3 font-medium">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="font-mono text-xs">
              {sessions.map((s: RTCSessionStats) => (
                <tr key={s.session_id} className="border-t border-zinc-100 dark:border-zinc-800">
                  <td className="py-1.5 pr-3">{s.session_id.slice(0, 12)}…</td>
                  <td className="py-1.5 pr-3">{s.mode}</td>
                  <td className="py-1.5 pr-3">{s.rtp_packets_in}</td>
                  <td className="py-1.5 pr-3">{s.packets_lost}</td>
                  <td className="py-1.5 pr-3">{(s.jitter_seconds * 1000).toFixed(1)} ms</td>
                  <td className="py-1.5 pr-3">{(s.app_rtt_micros / 1000).toFixed(1)} ms</td>
                  <td className="py-1.5 pr-3">{s.played_ms} ms</td>
                  <td className="py-1.5 pr-3">{s.frames_dropped}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

const columnHelper = createColumnHelper<AdminUser>();
const userColumns = [
  columnHelper.accessor("username", { header: "Username" }),
  columnHelper.accessor("role", { header: "Role" }),
  columnHelper.accessor("status", { header: "Status" }),
  columnHelper.accessor("created_at", {
    header: "Created",
    cell: (c) => new Date(c.getValue()).toLocaleDateString(),
  }),
];

// TanStack Table owns admin/user data grids (sorting/filtering/paging slot in
// here as the user set grows).
function UsersTable({ users }: { users: AdminUser[] }) {
  const table = useReactTable({
    data: users,
    columns: userColumns,
    getCoreRowModel: getCoreRowModel(),
  });
  return (
    <table className="w-full text-left text-sm">
      <thead className="text-zinc-500">
        {table.getHeaderGroups().map((hg) => (
          <tr key={hg.id}>
            {hg.headers.map((h) => (
              <th key={h.id} className="py-1 font-medium">
                {flexRender(h.column.columnDef.header, h.getContext())}
              </th>
            ))}
          </tr>
        ))}
      </thead>
      <tbody>
        {table.getRowModel().rows.map((row) => (
          <tr key={row.id} className="border-t border-zinc-100 dark:border-zinc-800">
            {row.getVisibleCells().map((cell) => (
              <td key={cell.id} className="py-1.5">
                {flexRender(cell.column.columnDef.cell, cell.getContext())}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
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
