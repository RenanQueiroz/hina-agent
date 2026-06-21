import { useEffect, useReducer, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, LogIn, Trash2 } from "lucide-react";
import type { AgentInfo, AgentLoginFrame } from "../lib/api.gen";
import { api, ApiError } from "../lib/api";
import {
  authTypeLabel,
  emptyLoginState,
  reduceLoginFrame,
} from "../lib/agents";
import { Button, Card, Input, Spinner } from "./ui";

// AgentsCard is the per-user callable-agent surface: authenticate Codex/Claude/
// Cursor (browser login or API key) and see which are eligible to run. Credentials
// are write-only — the server never returns a token, and the admin/user UI only ever
// sees a coarse status.
export function AgentsCard() {
  const catalog = useQuery({ queryKey: ["agents"], queryFn: api.listAgents });
  const [login, setLogin] = useState<{ provider: string; sessionId: string } | null>(null);

  if (catalog.isLoading) {
    return (
      <Card className="p-4">
        <Spinner />
      </Card>
    );
  }
  const data = catalog.data;
  if (!data) return null;

  return (
    <Card className="p-4">
      <h2 className="mb-1 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Coding agents
      </h2>
      <p className="mb-3 text-xs text-zinc-400">
        Authenticate a coding-agent CLI to call it as a sandboxed tool. Each run
        executes inside your isolated sandbox with your encrypted credentials — never
        on the host. Credentials are stored encrypted and never shown again.
      </p>
      {!data.enabled && (
        <p className="mb-3 rounded bg-amber-50 p-2 text-xs text-amber-700 dark:bg-amber-950 dark:text-amber-300">
          Callable agents are disabled on this server (<span className="font-mono">[agents] enabled</span>).
          You can still configure credentials below for when an admin enables them.
        </p>
      )}
      {data.enabled && !data.network_isolated && (
        <p className="mb-3 rounded bg-amber-50 p-2 text-xs text-amber-700 dark:bg-amber-950 dark:text-amber-300">
          Agent runs are paused until the operator asserts controlled sandbox egress
          (<span className="font-mono">[sandbox] network_isolated = true</span>).
        </p>
      )}
      <ul className="divide-y divide-zinc-100 dark:divide-zinc-800">
        {data.agents.map((a) => (
          <AgentRow
            key={a.provider}
            agent={a}
            browserAuthAvailable={data.browser_auth_available}
            onStartLogin={(sessionId) => setLogin({ provider: a.provider, sessionId })}
          />
        ))}
      </ul>
      {login && (
        <LoginDialog
          provider={login.provider}
          sessionId={login.sessionId}
          onClose={() => setLogin(null)}
        />
      )}
    </Card>
  );
}

function AgentRow({
  agent,
  browserAuthAvailable,
  onStartLogin,
}: {
  agent: AgentInfo;
  browserAuthAvailable: boolean;
  onStartLogin: (sessionId: string) => void;
}) {
  const qc = useQueryClient();
  const [showKey, setShowKey] = useState(false);
  const [err, setErr] = useState("");

  const logout = useMutation({
    mutationFn: () => api.logoutAgent(agent.provider),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["agents"] }),
  });
  const startLogin = useMutation({
    mutationFn: () => api.startAgentLogin(agent.provider, true),
    onSuccess: (r) => onStartLogin(r.session_id),
    onError: (e) => setErr(e instanceof ApiError ? e.message : "could not start login"),
  });

  return (
    <li className="py-3">
      <div className="flex items-center justify-between gap-2">
        <div>
          <span className="text-sm font-medium">{agent.display_name}</span>{" "}
          {agent.configured ? (
            <span className="text-xs text-emerald-600 dark:text-emerald-400">
              ✓ connected · {authTypeLabel(agent.configured_auth_type ?? "")}
            </span>
          ) : (
            <span className="text-xs text-zinc-400">not configured</span>
          )}
          <div className="text-xs text-zinc-400">
            {agent.runnable ? (
              <span className="text-emerald-600 dark:text-emerald-400">ready to run</span>
            ) : (
              agent.reason
            )}
          </div>
        </div>
        {agent.configured ? (
          <Button
            onClick={() => logout.mutate()}
            className="text-xs"
            title="Disconnect"
          >
            <Trash2 size={14} /> Disconnect
          </Button>
        ) : (
          <div className="flex gap-2">
            {agent.browser_auth && browserAuthAvailable && (
              <Button
                onClick={() => startLogin.mutate()}
                disabled={startLogin.isPending}
                className="text-xs"
              >
                <LogIn size={14} /> Sign in
              </Button>
            )}
            {!agent.local_only && (
              <Button onClick={() => setShowKey((v) => !v)} className="text-xs">
                API key
              </Button>
            )}
          </div>
        )}
      </div>
      {err && <p className="mt-1 text-xs text-red-500">{err}</p>}
      {showKey && !agent.configured && (
        <KeyForm
          agent={agent}
          onDone={() => {
            setShowKey(false);
            qc.invalidateQueries({ queryKey: ["agents"] });
          }}
        />
      )}
    </li>
  );
}

function KeyForm({ agent, onDone }: { agent: AgentInfo; onDone: () => void }) {
  const keyTypes = agent.auth_types.filter(
    (t) => t === "api_key" || t === "oauth_token",
  );
  const [authType, setAuthType] = useState<string>(keyTypes[0] ?? "api_key");
  const [value, setValue] = useState("");
  const [err, setErr] = useState("");

  const save = useMutation({
    mutationFn: () => api.setAgentKey(agent.provider, authType, value),
    onSuccess: onDone,
    onError: (e) => setErr(e instanceof ApiError ? e.message : "could not save"),
  });

  return (
    <div className="mt-2 flex flex-wrap items-center gap-2">
      {keyTypes.length > 1 && (
        <select
          value={authType}
          onChange={(e) => setAuthType(e.target.value)}
          className="rounded border border-zinc-300 bg-transparent px-2 py-1 text-xs dark:border-zinc-700"
        >
          {keyTypes.map((t) => (
            <option key={t} value={t}>
              {authTypeLabel(t)}
            </option>
          ))}
        </select>
      )}
      <Input
        type="password"
        placeholder="paste credential"
        value={value}
        onChange={(e) => setValue(e.target.value)}
        className="w-64 text-xs"
      />
      <Button
        onClick={() => save.mutate()}
        disabled={!value || save.isPending}
        className="text-xs"
      >
        Save
      </Button>
      {err && <span className="text-xs text-red-500">{err}</span>}
    </div>
  );
}

// LoginDialog streams the sanitized output of an interactive browser login,
// surfacing the login URL + device code and a paste box. It never displays a token.
function LoginDialog({
  provider,
  sessionId,
  onClose,
}: {
  provider: string;
  sessionId: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [state, dispatch] = useReducer(reduceLoginFrame, emptyLoginState);
  const [paste, setPaste] = useState("");

  useEffect(() => {
    const es = new EventSource(api.agentLoginEventsUrl(sessionId), {
      withCredentials: true,
    });
    es.onmessage = (e) => {
      try {
        dispatch(JSON.parse(e.data) as AgentLoginFrame);
      } catch {
        /* ignore malformed frame */
      }
    };
    return () => es.close();
  }, [sessionId]);

  useEffect(() => {
    if (state.done) qc.invalidateQueries({ queryKey: ["agents"] });
  }, [state.done, qc]);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
      <Card className="w-full max-w-lg space-y-3 p-4">
        <h3 className="text-sm font-semibold">Sign in to {provider}</h3>
        {state.urls.map((u) => (
          <a
            key={u}
            href={u}
            target="_blank"
            rel="noreferrer"
            className="flex items-center gap-1 break-all text-sm text-blue-600 underline dark:text-blue-400"
          >
            <ExternalLink size={14} /> {u}
          </a>
        ))}
        {state.codes.length > 0 && (
          <div className="text-sm">
            Code:{" "}
            {state.codes.map((c) => (
              <span key={c} className="mr-2 rounded bg-zinc-100 px-2 py-0.5 font-mono dark:bg-zinc-800">
                {c}
              </span>
            ))}
          </div>
        )}
        <pre className="max-h-48 overflow-auto rounded bg-zinc-50 p-2 text-xs text-zinc-600 dark:bg-zinc-900 dark:text-zinc-400">
          {state.lines.join("\n") || "Starting login…"}
        </pre>
        {state.needsInput && !state.done && (
          <div className="flex gap-2">
            <Input
              placeholder="paste code"
              value={paste}
              onChange={(e) => setPaste(e.target.value)}
              className="flex-1 text-sm"
            />
            <Button
              onClick={() => {
                api.sendAgentLoginInput(sessionId, paste).catch(() => {});
                setPaste("");
              }}
              className="text-sm"
            >
              Send
            </Button>
          </div>
        )}
        {state.done ? (
          <div className="flex items-center justify-between">
            <span
              className={
                state.done.ok
                  ? "text-sm text-emerald-600 dark:text-emerald-400"
                  : "text-sm text-red-500"
              }
            >
              {state.done.ok ? "Signed in." : `Login failed: ${state.done.error}`}
            </span>
            <Button onClick={onClose} className="text-sm">
              Close
            </Button>
          </div>
        ) : (
          <div className="flex justify-end">
            <Button
              onClick={() => {
                api.cancelAgentLogin(sessionId).catch(() => {});
                onClose();
              }}
              className="text-sm"
            >
              Cancel
            </Button>
          </div>
        )}
      </Card>
    </div>
  );
}
