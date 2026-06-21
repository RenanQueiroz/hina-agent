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

      <RuntimePanel />
      <ASRRuntimePanel />
      <VADRuntimePanel />

      <Card className="p-4">
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">Users</h2>
        {users.isLoading ? <Spinner /> : <UsersTable users={users.data ?? []} />}
      </Card>

      <SandboxPanel />
      <AgentsPanel />
      <LiveSessionsPanel />
      <LogsPanel />
    </div>
  );
}

// AgentsPanel shows the Phase 8 callable-agent availability and each user's coarse
// profile status — never a token, URL, or device code.
function AgentsPanel() {
  const ag = useQuery({
    queryKey: ["admin", "agents"],
    queryFn: api.adminAgents,
    refetchInterval: 5000,
  });
  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Callable agents
      </h2>
      {ag.isLoading ? (
        <Spinner />
      ) : (
        <>
          <dl className="mb-4 grid grid-cols-[160px_1fr] gap-y-1 text-sm">
            <dt className="text-zinc-500">Status</dt>
            <dd>
              {ag.data?.available ? "available" : "unavailable"}
              {ag.data?.reason ? (
                <span className="text-zinc-400"> — {ag.data.reason}</span>
              ) : null}
            </dd>
            <dt className="text-zinc-500">Browser login</dt>
            <dd>{ag.data?.browser_auth_available ? "available" : "unavailable"}</dd>
          </dl>
          {(ag.data?.profiles ?? []).length === 0 ? (
            <p className="text-sm text-zinc-400">No agents configured by any user.</p>
          ) : (
            <table className="w-full text-left text-xs">
              <thead className="text-zinc-500">
                <tr>
                  {["User", "Agent", "Auth type", "Status"].map((h) => (
                    <th key={h} className="py-1 pr-3 font-medium">
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {(ag.data?.profiles ?? []).map((p) => (
                  <tr
                    key={`${p.user_id}:${p.provider}`}
                    className="border-t border-zinc-100 dark:border-zinc-800"
                  >
                    <td className="py-1.5 pr-3">{p.username}</td>
                    <td className="py-1.5 pr-3">{p.provider}</td>
                    <td className="py-1.5 pr-3">{p.auth_type}</td>
                    <td className="py-1.5 pr-3">{p.status}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </Card>
  );
}

// SandboxPanel shows the Phase 7 sbx runner status, per-user storage/run usage,
// and the recent tool-run audit log (no secret values; commands are redacted).
function SandboxPanel() {
  const sb = useQuery({
    queryKey: ["admin", "sandbox"],
    queryFn: api.adminSandbox,
    refetchInterval: 5000,
  });
  const rt = sb.data?.runtime;
  const status = !rt?.enabled ? "disabled" : rt?.available ? "available" : "unavailable";
  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Sandbox runtime &amp; usage
      </h2>
      {sb.isLoading ? (
        <Spinner />
      ) : (
        <>
          <dl className="mb-4 grid grid-cols-[140px_1fr] gap-y-1 text-sm">
            <dt className="text-zinc-500">Status</dt>
            <dd>{status}</dd>
            <dt className="text-zinc-500">sbx version</dt>
            <dd>
              {rt?.version ? `${rt.version} (pinned ${rt.pinned})` : (
                <span className="text-zinc-400">{rt?.reason || "not installed"}</span>
              )}
            </dd>
            <dt className="text-zinc-500">Approval</dt>
            <dd>{rt?.approval}</dd>
          </dl>

          <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-zinc-400">
            Per-user usage
          </h3>
          <table className="mb-4 w-full text-left text-sm">
            <thead className="text-zinc-500">
              <tr>
                {["User", "Workspace", "Tool runs"].map((h) => (
                  <th key={h} className="py-1 pr-3 font-medium">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {(sb.data?.users ?? []).map((u) => (
                <tr key={u.user_id} className="border-t border-zinc-100 dark:border-zinc-800">
                  <td className="py-1.5 pr-3">{u.username}</td>
                  <td className="py-1.5 pr-3">{(u.workspace_bytes / 1024).toFixed(0)} KiB</td>
                  <td className="py-1.5 pr-3">{u.run_count}</td>
                </tr>
              ))}
            </tbody>
          </table>

          <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-zinc-400">
            Recent tool runs
          </h3>
          {(sb.data?.runs ?? []).length === 0 ? (
            <p className="text-sm text-zinc-400">No tool runs yet.</p>
          ) : (
            <div className="max-h-64 overflow-auto">
              <table className="w-full text-left text-xs">
                <thead className="text-zinc-500">
                  <tr>
                    {["When", "Tool", "Decision", "Exit", "Command", "Status / error"].map((h) => (
                      <th key={h} className="py-1 pr-3 font-medium">
                        {h}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody className="font-mono">
                  {(sb.data?.runs ?? []).map((r) => (
                    <tr key={r.id} className="border-t border-zinc-100 dark:border-zinc-800">
                      <td className="py-1 pr-3">{new Date(r.created_at).toLocaleTimeString()}</td>
                      <td className="py-1 pr-3">{r.tool}</td>
                      <td className="py-1 pr-3">{r.decision}</td>
                      <td className="py-1 pr-3">{r.exit_code}</td>
                      <td className="py-1 pr-3 max-w-xs truncate">{r.command}</td>
                      <td className="py-1 pr-3 max-w-xs truncate">
                        {r.error ? (
                          <span className="text-red-500">{r.error}</span>
                        ) : r.exit_code === 0 ? (
                          <span className="text-green-600">ok</span>
                        ) : (
                          <span className="text-zinc-400">—</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </Card>
  );
}

// RuntimePanel shows the local-inference backend: ONNX Runtime version/provider/
// lib path and the TTS engine's load state + cold/warm synth latency (Phase 4).
function RuntimePanel() {
  const rt = useQuery({
    queryKey: ["admin", "runtime"],
    queryFn: api.adminRuntime,
    refetchInterval: 5000,
  });
  const tts = rt.data?.tts;
  const ort = tts?.runtime;
  const status = !tts?.enabled
    ? "disabled"
    : tts?.available
      ? tts.loaded
        ? "loaded (warm)"
        : "ready (idle)"
      : "unavailable";
  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Local TTS runtime
      </h2>
      {rt.isLoading ? (
        <Spinner />
      ) : (
        <dl className="grid grid-cols-[140px_1fr] gap-y-1 text-sm">
          <dt className="text-zinc-500">Status</dt>
          <dd>{status}</dd>
          <dt className="text-zinc-500">ONNX Runtime</dt>
          <dd>
            {ort?.available ? (
              `${ort.version} (${ort.provider})`
            ) : (
              <span className="text-zinc-400">{ort?.reason || "not linked"}</span>
            )}
          </dd>
          <dt className="text-zinc-500">Library</dt>
          <dd className="truncate font-mono text-xs">
            {ort?.lib_path || <span className="text-zinc-400">—</span>}
          </dd>
          <dt className="text-zinc-500">Voice / Lang</dt>
          <dd>
            {tts?.voice || "—"} / {tts?.lang || "—"}
          </dd>
          <dt className="text-zinc-500">Cold load</dt>
          <dd>{tts?.cold_load_ms ? `${tts.cold_load_ms} ms` : "—"}</dd>
          <dt className="text-zinc-500">Last synth</dt>
          <dd>{tts?.last_synth_ms ? `${tts.last_synth_ms} ms` : "—"}</dd>
          <dt className="text-zinc-500">Synths / errors</dt>
          <dd>
            {tts?.synth_count ?? 0} / {tts?.error_count ?? 0}
          </dd>
          {tts?.reason && !tts.available && (
            <>
              <dt className="text-zinc-500">Reason</dt>
              <dd className="text-amber-600 dark:text-amber-400">{tts.reason}</dd>
            </>
          )}
          {tts?.last_error && (
            <>
              <dt className="text-zinc-500">Last error</dt>
              <dd className="text-red-500">{tts.last_error}</dd>
            </>
          )}
        </dl>
      )}
    </Card>
  );
}

// ASRRuntimePanel shows the local streaming ASR (Nemotron) engine: ONNX Runtime,
// load state, language, name-biasing, and cold/chunk latency (Phase 5).
function ASRRuntimePanel() {
  const rt = useQuery({
    queryKey: ["admin", "runtime"],
    queryFn: api.adminRuntime,
    refetchInterval: 5000,
  });
  const asr = rt.data?.asr;
  const ort = asr?.runtime;
  const status = !asr?.enabled
    ? "disabled"
    : asr?.available
      ? asr.loaded
        ? "loaded (warm)"
        : "ready (idle)"
      : "unavailable";
  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Local ASR runtime
      </h2>
      {rt.isLoading ? (
        <Spinner />
      ) : (
        <dl className="grid grid-cols-[140px_1fr] gap-y-1 text-sm">
          <dt className="text-zinc-500">Status</dt>
          <dd>{status}</dd>
          <dt className="text-zinc-500">ONNX Runtime</dt>
          <dd>
            {ort?.available ? (
              `${ort.version} (${ort.provider})`
            ) : (
              <span className="text-zinc-400">{ort?.reason || "not linked"}</span>
            )}
          </dd>
          <dt className="text-zinc-500">Language</dt>
          <dd>{asr?.language || "—"}</dd>
          <dt className="text-zinc-500">Name biasing</dt>
          <dd>{asr?.biasing ? "on" : "off"}</dd>
          <dt className="text-zinc-500">Cold load</dt>
          <dd>{asr?.cold_load_ms ? `${asr.cold_load_ms} ms` : "—"}</dd>
          <dt className="text-zinc-500">Last chunk</dt>
          <dd>{asr?.last_chunk_ms ? `${asr.last_chunk_ms} ms` : "—"}</dd>
          <dt className="text-zinc-500">Chunks / errors</dt>
          <dd>
            {asr?.chunk_count ?? 0} / {asr?.error_count ?? 0}
          </dd>
          {asr?.reason && !asr.available && (
            <>
              <dt className="text-zinc-500">Reason</dt>
              <dd className="text-amber-600 dark:text-amber-400">{asr.reason}</dd>
            </>
          )}
          {asr?.last_error && (
            <>
              <dt className="text-zinc-500">Last error</dt>
              <dd className="text-red-500">{asr.last_error}</dd>
            </>
          )}
        </dl>
      )}
    </Card>
  );
}

// VADRuntimePanel shows the local Silero VAD engine that powers the Phase 6 live
// conversation loop: ONNX Runtime, load state, and probe/cold-load metrics.
function VADRuntimePanel() {
  const rt = useQuery({
    queryKey: ["admin", "runtime"],
    queryFn: api.adminRuntime,
    refetchInterval: 5000,
  });
  const vad = rt.data?.vad;
  const ort = vad?.runtime;
  const status = !vad?.enabled
    ? "disabled"
    : vad?.available
      ? vad.loaded
        ? "loaded (warm)"
        : "ready (idle)"
      : "unavailable";
  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Live voice / VAD runtime
      </h2>
      {rt.isLoading ? (
        <Spinner />
      ) : (
        <dl className="grid grid-cols-[140px_1fr] gap-y-1 text-sm">
          <dt className="text-zinc-500">Status</dt>
          <dd>{status}</dd>
          <dt className="text-zinc-500">ONNX Runtime</dt>
          <dd>
            {ort?.available ? (
              `${ort.version} (${ort.provider})`
            ) : (
              <span className="text-zinc-400">{ort?.reason || "not linked"}</span>
            )}
          </dd>
          <dt className="text-zinc-500">Cold load</dt>
          <dd>{vad?.cold_load_ms ? `${vad.cold_load_ms} ms` : "—"}</dd>
          <dt className="text-zinc-500">Probes / errors</dt>
          <dd>
            {vad?.probe_count ?? 0} / {vad?.error_count ?? 0}
          </dd>
          {vad?.reason && !vad.available && (
            <>
              <dt className="text-zinc-500">Reason</dt>
              <dd className="text-amber-600 dark:text-amber-400">{vad.reason}</dd>
            </>
          )}
          {vad?.last_error && (
            <>
              <dt className="text-zinc-500">Last error</dt>
              <dd className="text-red-500">{vad.last_error}</dd>
            </>
          )}
        </dl>
      )}
    </Card>
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
                {["Session", "Mode", "Pkts in", "Loss", "Jitter", "RTT", "Played", "Drops", "Lost turns"].map((h) => (
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
                  <td className="py-1.5 pr-3">{s.dropped_turns}</td>
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
