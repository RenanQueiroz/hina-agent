import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Save, Trash2 } from "lucide-react";
import type {
  SandboxEnvironment,
  SandboxNetworkRule,
  SandboxSecretGrant,
} from "../lib/api.gen";
import { api, ApiError } from "../lib/api";
import { humanTool, isValidEnvName, toggleTool } from "../lib/sandbox";
import { Button, Card, Input, Spinner } from "../components/ui";

// SandboxPage is the per-user Sandbox Environment surface: the secret vault and
// the policy (allowed tools, network allow-list, secret grants) that constrains
// what this user's sandboxes may do — independent of any one chat/Automation.
export function SandboxPage() {
  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <div>
        <h1 className="text-lg font-semibold">Sandbox environment</h1>
        <p className="text-sm text-zinc-500">
          Tools the assistant runs execute inside your isolated sandbox. Manage your
          secrets and the policy that governs them here.
        </p>
      </div>
      <SecretsCard />
      <EnvironmentCard />
    </div>
  );
}

function SecretsCard() {
  const qc = useQueryClient();
  const secrets = useQuery({ queryKey: ["secrets"], queryFn: api.listSecrets });
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [description, setDescription] = useState("");
  const [err, setErr] = useState("");

  const create = useMutation({
    mutationFn: () => api.createSecret(name.trim(), value, description.trim()),
    onSuccess: () => {
      setName("");
      setValue("");
      setDescription("");
      setErr("");
      qc.invalidateQueries({ queryKey: ["secrets"] });
    },
    onError: (e) => setErr(e instanceof ApiError ? e.message : "could not add secret"),
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.deleteSecret(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["secrets"] }),
  });

  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Secrets
      </h2>
      <p className="mb-3 text-xs text-zinc-400">
        Stored encrypted; values are never shown again and never leave the server in
        plaintext. Grant one below to inject it into tool runs — secrets are only
        injected when the server's sandbox network is isolated
        (<span className="font-mono">[sandbox] network_isolated</span>).
      </p>
      {secrets.isLoading ? (
        <Spinner />
      ) : secrets.data && secrets.data.length > 0 ? (
        <ul className="mb-4 divide-y divide-zinc-100 dark:divide-zinc-800">
          {secrets.data.map((s) => (
            <li key={s.id} className="flex items-center gap-2 py-2 text-sm">
              <span className="font-mono">{s.name}</span>
              {s.description && (
                <span className="text-zinc-400">— {s.description}</span>
              )}
              <Button
                variant="ghost"
                className="ml-auto !px-2 !py-1 text-red-500"
                aria-label={`Delete ${s.name}`}
                onClick={() => remove.mutate(s.id)}
              >
                <Trash2 size={14} />
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="mb-4 text-sm text-zinc-400">No secrets yet.</p>
      )}

      <form
        className="grid grid-cols-1 gap-2 sm:grid-cols-[1fr_1fr_auto]"
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim() && value) create.mutate();
        }}
      >
        <Input
          placeholder="NAME"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <Input
          type="password"
          placeholder="value"
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
        <Button type="submit" disabled={create.isPending || !name.trim() || !value}>
          <Plus size={16} /> Add
        </Button>
        <Input
          className="sm:col-span-3"
          placeholder="description (optional)"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </form>
      {err && <p className="mt-2 text-sm text-red-600">{err}</p>}
    </Card>
  );
}

function EnvironmentCard() {
  const qc = useQueryClient();
  const envQuery = useQuery({
    queryKey: ["sandbox-environment"],
    queryFn: api.getSandboxEnvironment,
  });
  const [env, setEnv] = useState<SandboxEnvironment | null>(null);
  const [saved, setSaved] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    if (envQuery.data) setEnv(envQuery.data);
  }, [envQuery.data]);

  const save = useMutation({
    mutationFn: (e: SandboxEnvironment) => api.updateSandboxEnvironment(e),
    onSuccess: (e) => {
      setEnv(e);
      setSaved(true);
      setErr("");
      qc.invalidateQueries({ queryKey: ["sandbox-environment"] });
    },
    onError: (e) => setErr(e instanceof ApiError ? e.message : "could not save"),
  });

  if (envQuery.isLoading || !env) {
    return (
      <Card className="p-4">
        <Spinner />
      </Card>
    );
  }

  const update = (patch: Partial<SandboxEnvironment>) => {
    setEnv({ ...env, ...patch });
    setSaved(false);
  };
  const updateNetwork = (patch: Partial<SandboxEnvironment["network"]>) =>
    update({ network: { ...env.network, ...patch } });

  const grantInvalid = (env.secret_grants ?? []).some(
    (g) => g.env_name && !isValidEnvName(g.env_name),
  );

  return (
    <Card className="p-4">
      <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-500">
        Policy
      </h2>

      {/* Allowed tools */}
      <fieldset className="mb-4">
        <legend className="mb-1 text-sm font-medium">Allowed tools</legend>
        <div className="flex flex-col gap-1">
          {(env.available_tools ?? []).map((tool) => (
            <label key={tool} className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={(env.allowed_tools ?? []).includes(tool)}
                onChange={() =>
                  update({ allowed_tools: toggleTool(env.allowed_tools ?? [], tool) })
                }
              />
              {humanTool(tool)}
            </label>
          ))}
        </div>
      </fieldset>

      {/* Network */}
      <fieldset className="mb-4">
        <legend className="mb-1 text-sm font-medium">Network</legend>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={env.network?.default === "allow"}
            onChange={(e) =>
              updateNetwork({ default: e.target.checked ? "allow" : "deny" })
            }
          />
          Allow all outbound network (not recommended)
        </label>
        {env.network?.default !== "allow" && (
          <RuleEditor
            rules={env.network?.allow ?? []}
            onChange={(allow) => updateNetwork({ allow })}
          />
        )}
      </fieldset>

      {/* Secret grants */}
      <fieldset className="mb-4">
        <legend className="mb-1 text-sm font-medium">Secret grants</legend>
        <p className="mb-1 text-xs text-zinc-400">
          Inject a vaulted secret into tool runs as an environment variable.
        </p>
        <GrantEditor
          grants={env.secret_grants ?? []}
          onChange={(secret_grants) => update({ secret_grants })}
        />
      </fieldset>

      <div className="flex items-center gap-3">
        <Button
          onClick={() => save.mutate(env)}
          disabled={save.isPending || grantInvalid}
        >
          <Save size={16} /> {save.isPending ? "Saving…" : "Save policy"}
        </Button>
        {saved && <span className="text-sm text-green-600">Saved.</span>}
        {grantInvalid && (
          <span className="text-sm text-red-600">Fix invalid env names first.</span>
        )}
        {err && <span className="text-sm text-red-600">{err}</span>}
      </div>
    </Card>
  );
}

function RuleEditor({
  rules,
  onChange,
}: {
  rules: SandboxNetworkRule[];
  onChange: (r: SandboxNetworkRule[]) => void;
}) {
  return (
    <div className="mt-2 space-y-2">
      {rules.map((r, i) => (
        <div key={i} className="flex items-center gap-2">
          <Input
            className="flex-1"
            placeholder="host (e.g. localhost)"
            value={r.host}
            onChange={(e) =>
              onChange(rules.map((x, j) => (j === i ? { ...x, host: e.target.value } : x)))
            }
          />
          <Input
            className="w-28"
            type="number"
            placeholder="port"
            value={r.port || ""}
            onChange={(e) =>
              onChange(
                rules.map((x, j) =>
                  j === i ? { ...x, port: Number(e.target.value) } : x,
                ),
              )
            }
          />
          <Button
            variant="ghost"
            className="!px-2 !py-1 text-red-500"
            aria-label="Remove rule"
            onClick={() => onChange(rules.filter((_, j) => j !== i))}
          >
            <Trash2 size={14} />
          </Button>
        </div>
      ))}
      <Button
        variant="ghost"
        className="!px-2 !py-1"
        onClick={() => onChange([...rules, { host: "", port: 0 }])}
      >
        <Plus size={14} /> Add host:port
      </Button>
    </div>
  );
}

function GrantEditor({
  grants,
  onChange,
}: {
  grants: SandboxSecretGrant[];
  onChange: (g: SandboxSecretGrant[]) => void;
}) {
  const secrets = useQuery({ queryKey: ["secrets"], queryFn: api.listSecrets });
  return (
    <div className="space-y-2">
      {grants.map((g, i) => (
        <div key={i} className="flex items-center gap-2">
          <select
            className="flex-1 rounded-md border border-zinc-300 bg-white px-2 py-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
            value={g.secret_id}
            onChange={(e) =>
              onChange(
                grants.map((x, j) =>
                  j === i ? { ...x, secret_id: e.target.value } : x,
                ),
              )
            }
          >
            <option value="">select secret…</option>
            {(secrets.data ?? []).map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
              </option>
            ))}
          </select>
          <Input
            className="w-40"
            placeholder="ENV_NAME"
            value={g.env_name}
            onChange={(e) =>
              onChange(
                grants.map((x, j) =>
                  j === i ? { ...x, env_name: e.target.value } : x,
                ),
              )
            }
          />
          <Button
            variant="ghost"
            className="!px-2 !py-1 text-red-500"
            aria-label="Remove grant"
            onClick={() => onChange(grants.filter((_, j) => j !== i))}
          >
            <Trash2 size={14} />
          </Button>
        </div>
      ))}
      <Button
        variant="ghost"
        className="!px-2 !py-1"
        onClick={() => onChange([...grants, { secret_id: "", env_name: "" }])}
      >
        <Plus size={14} /> Add grant
      </Button>
    </div>
  );
}
