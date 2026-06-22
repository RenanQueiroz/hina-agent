import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Play, Trash2, Download, Upload, Sparkles, ChevronUp, ChevronDown, History, Pencil, X } from "lucide-react";
import { api, ApiError } from "../lib/api";
import { Button, Card, Input, Spinner } from "../components/ui";
import type { AutomationMeta, AutomationValidation } from "../lib/api.gen";
import {
  type AutomationDef,
  type AutomationStep,
  STEP_TYPES,
  addStep,
  defaultDefinition,
  emptyStep,
  moveStep,
  parseDefinition,
  prReviewTemplate,
  removeStepAt,
  statusTone,
  toggleInList,
  uniqueStepId,
  updateStepAt,
} from "../lib/automations";

type View = { kind: "list" } | { kind: "builder"; id?: string } | { kind: "runs"; id: string; name: string };

export function AutomationsPage() {
  const [view, setView] = useState<View>({ kind: "list" });
  const meta = useQuery({ queryKey: ["automation-meta"], queryFn: api.automationMeta });

  if (meta.isLoading) return <Spinner />;
  if (!meta.data?.available) {
    return (
      <div className="mx-auto max-w-2xl p-6">
        <Card className="p-6 text-sm text-zinc-600 dark:text-zinc-300">
          Automations are not enabled on this server. {meta.data?.reason}
        </Card>
      </div>
    );
  }
  return (
    <div className="mx-auto max-w-4xl p-4">
      {view.kind === "list" && <List meta={meta.data} setView={setView} />}
      {view.kind === "builder" && <Builder meta={meta.data} id={view.id} onDone={() => setView({ kind: "list" })} />}
      {view.kind === "runs" && <Runs id={view.id} name={view.name} onBack={() => setView({ kind: "list" })} />}
    </div>
  );
}

function List({ meta, setView }: { meta: AutomationMeta; setView: (v: View) => void }) {
  const qc = useQueryClient();
  const list = useQuery({ queryKey: ["automations"], queryFn: api.listAutomations });
  const invalidate = () => qc.invalidateQueries({ queryKey: ["automations"] });

  const toggle = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => api.setAutomationEnabled(id, enabled),
    onSuccess: invalidate,
  });
  const del = useMutation({ mutationFn: (id: string) => api.deleteAutomation(id), onSuccess: invalidate });
  const run = useMutation({ mutationFn: (id: string) => api.runAutomation(id) });

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <h1 className="text-lg font-semibold">Automations</h1>
        <div className="ml-auto flex gap-2">
          <Button variant="ghost" onClick={() => setView({ kind: "builder" })}>
            <Plus size={16} /> New
          </Button>
        </div>
      </div>
      {!meta.network_isolated && (
        <Card className="p-3 text-xs text-amber-700 dark:text-amber-400">
          [sandbox] network_isolated is off — automations that use callable agents or inject secrets can't be enabled.
        </Card>
      )}
      {list.isLoading ? (
        <Spinner />
      ) : (list.data ?? []).length === 0 ? (
        <Card className="p-6 text-sm text-zinc-500">No automations yet. Create one to get started.</Card>
      ) : (
        <div className="space-y-2">
          {(list.data ?? []).map((a) => (
            <Card key={a.id} className="flex items-center gap-3 p-3">
              <label className="flex cursor-pointer items-center gap-2" title="Enable/disable">
                <input
                  type="checkbox"
                  checked={a.enabled}
                  onChange={(e) => toggle.mutate({ id: a.id, enabled: e.target.checked })}
                />
              </label>
              <div className="min-w-0">
                <div className="truncate font-medium">{a.name}</div>
                <div className="text-xs text-zinc-500">
                  {a.trigger}
                  {a.next_run ? ` · next ${new Date(a.next_run).toLocaleString()}` : ""}
                  {a.last_status ? ` · last ${a.last_status}` : ""}
                </div>
              </div>
              <div className="ml-auto flex gap-1">
                <Button
                  variant="ghost"
                  title={a.enabled ? "Run now" : "Enable (review) the automation before running it"}
                  disabled={!a.enabled}
                  onClick={() => run.mutate(a.id)}
                >
                  <Play size={15} />
                </Button>
                <Button variant="ghost" title="Run history" onClick={() => setView({ kind: "runs", id: a.id, name: a.name })}>
                  <History size={15} />
                </Button>
                <Button variant="ghost" title="Edit" onClick={() => setView({ kind: "builder", id: a.id })}>
                  <Pencil size={15} />
                </Button>
                <a href={api.automationExportUrl(a.id)} title="Export JSON" className="inline-flex items-center rounded-md px-3 py-2 text-zinc-700 hover:bg-zinc-100 dark:text-zinc-200 dark:hover:bg-zinc-800">
                  <Download size={15} />
                </a>
                <Button variant="ghost" title="Delete" onClick={() => confirm(`Delete "${a.name}"?`) && del.mutate(a.id)}>
                  <Trash2 size={15} />
                </Button>
              </div>
            </Card>
          ))}
        </div>
      )}
      {toggle.isError && (
        <Card className="p-3 text-xs text-red-600">{(toggle.error as ApiError)?.message ?? "Could not enable"}</Card>
      )}
      {run.isSuccess && <Card className="p-3 text-xs text-emerald-600">Run started.</Card>}
    </div>
  );
}

function Builder({ meta, id, onDone }: { meta: AutomationMeta; id?: string; onDone: () => void }) {
  const qc = useQueryClient();
  const existing = useQuery({ queryKey: ["automation", id], queryFn: () => api.getAutomation(id!), enabled: !!id });
  const [def, setDef] = useState<AutomationDef>(defaultDefinition);
  const [loadedFor, setLoadedFor] = useState<string | undefined>(undefined);
  const [validation, setValidation] = useState<AutomationValidation | null>(null);
  const [importText, setImportText] = useState("");
  const [importErr, setImportErr] = useState("");
  const [assist, setAssist] = useState("");
  const [showJson, setShowJson] = useState(false);

  // Load an existing definition into the form once (the detail's `definition` is the
  // parsed automation.v1 object).
  if (id && existing.data && loadedFor !== id) {
    setDef(existing.data.definition as unknown as AutomationDef);
    setLoadedFor(id);
  }

  const validate = useMutation({
    mutationFn: () => api.validateAutomation(def),
    onSuccess: (v) => setValidation(v),
  });
  const save = useMutation({
    mutationFn: () => (id ? api.updateAutomation(id, def) : api.createAutomation(def)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["automations"] });
      onDone();
    },
  });
  const assistMut = useMutation({
    mutationFn: () => api.assistAutomation(assist),
    onSuccess: (r) => {
      // r.definition is "always valid JSON" but may be null when the model never returned a
      // parseable automation.v1 — only adopt it as the draft when it is genuinely a v1 object,
      // otherwise KEEP the current draft and surface the failure (raw_text + issues) so a
      // degraded LLM response can't blank the builder by feeding null into render.
      const d = r.definition as { schema_version?: string } | null;
      const issues = [...(r.issues ?? [])];
      if (d && typeof d === "object" && d.schema_version === "automation.v1") {
        setDef(d as unknown as AutomationDef);
      } else if (r.raw_text) {
        issues.push({ path: "assist", message: `the model did not return a valid automation.v1 document; raw output: ${r.raw_text.slice(0, 300)}` });
      } else {
        issues.push({ path: "assist", message: "the model did not return a valid automation.v1 document" });
      }
      setValidation({ valid: r.valid, issues, eligible: false });
    },
  });

  const update = (patch: Partial<AutomationDef>) => setDef((d) => ({ ...d, ...patch }));
  const setSteps = (steps: AutomationStep[]) => setDef((d) => ({ ...d, steps }));

  const doImport = () => {
    const { def: parsed, error } = parseDefinition(importText);
    if (error || !parsed) {
      setImportErr(error ?? "invalid");
      return;
    }
    setDef(parsed);
    setImportText("");
    setImportErr("");
  };

  if (id && existing.isLoading) return <Spinner />;

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <h1 className="text-lg font-semibold">{id ? "Edit automation" : "New automation"}</h1>
        <div className="ml-auto flex gap-2">
          <Button variant="ghost" onClick={() => setDef(prReviewTemplate())} title="Load the PR-review template">
            <Sparkles size={15} /> PR-review template
          </Button>
          <Button variant="ghost" onClick={onDone}>
            <X size={15} /> Cancel
          </Button>
        </div>
      </div>

      {/* LLM-assisted draft */}
      <Card className="space-y-2 p-3">
        <div className="text-sm font-medium">Draft with AI</div>
        <div className="flex gap-2">
          <Input placeholder="Describe what this automation should do…" value={assist} onChange={(e) => setAssist(e.target.value)} />
          <Button onClick={() => assistMut.mutate()} disabled={!assist.trim() || assistMut.isPending}>
            <Sparkles size={15} /> Draft
          </Button>
        </div>
        {assistMut.isPending && <div className="text-xs text-zinc-500">Drafting… (validating + retrying)</div>}
        {assistMut.data && (
          <div className="text-xs text-zinc-500">
            Draft loaded after {assistMut.data.attempts} attempt(s){assistMut.data.valid ? " — schema-valid" : " — review the issues below"}. Review before enabling.
          </div>
        )}
      </Card>

      <Section title="Basics">
        <Field label="Name">
          <Input value={def.name} onChange={(e) => update({ name: e.target.value })} />
        </Field>
        <Field label="Description">
          <Input value={def.description ?? ""} onChange={(e) => update({ description: e.target.value })} />
        </Field>
      </Section>

      <Section title="Trigger">
        <Field label="Type">
          <Select value={def.trigger.type} onChange={(v) => update({ trigger: { ...def.trigger, type: v as AutomationDef["trigger"]["type"] } })} options={["manual", "interval", "cron"]} />
        </Field>
        {def.trigger.type === "interval" && (
          <Field label="Every (e.g. 5m)">
            <Input value={def.trigger.every ?? ""} onChange={(e) => update({ trigger: { ...def.trigger, every: e.target.value } })} />
          </Field>
        )}
        {def.trigger.type === "cron" && (
          <>
            <Field label="Cron (5 fields)">
              <Input value={def.trigger.cron ?? ""} placeholder="0 9 * * *" onChange={(e) => update({ trigger: { ...def.trigger, cron: e.target.value } })} />
            </Field>
            <Field label="Timezone">
              <Input value={def.timezone ?? ""} placeholder="America/New_York" onChange={(e) => update({ timezone: e.target.value })} />
            </Field>
          </>
        )}
        <Field label="Missed-run policy">
          <Select value={def.missed_run_policy ?? "skip"} onChange={(v) => update({ missed_run_policy: v as "skip" | "run_once" })} options={["skip", "run_once"]} />
        </Field>
      </Section>

      <Section title="Concurrency & budget">
        <Field label="Concurrency">
          <Select value={def.concurrency.policy} onChange={(v) => update({ concurrency: { ...def.concurrency, policy: v } })} options={["skip_if_running", "queue_one", "parallel", "cancel_previous"]} />
        </Field>
        {def.concurrency.policy === "parallel" && (
          <Field label="Max parallel">
            <NumberInput value={def.concurrency.max_parallel ?? 1} onChange={(n) => update({ concurrency: { ...def.concurrency, max_parallel: n } })} />
          </Field>
        )}
        <Field label="Timeout (s)">
          <NumberInput value={def.budget.timeout_seconds ?? 0} onChange={(n) => update({ budget: { ...def.budget, timeout_seconds: n } })} />
        </Field>
        <Field label="Max model calls">
          <NumberInput value={def.budget.max_model_calls ?? 0} onChange={(n) => update({ budget: { ...def.budget, max_model_calls: n } })} />
        </Field>
        <Field label="Max agent runs">
          <NumberInput value={def.budget.max_agent_runs ?? 0} onChange={(n) => update({ budget: { ...def.budget, max_agent_runs: n } })} />
        </Field>
      </Section>

      <Section title="Sandbox permission profile">
        <Field label="Mode">
          <Select value={def.sandbox.mode} onChange={(v) => update({ sandbox: { ...def.sandbox, mode: v as "granular" | "unrestricted" } })} options={["granular", "unrestricted"]} />
        </Field>
        <Field label="Network">
          <Select value={def.sandbox.network ?? "disabled"} onChange={(v) => update({ sandbox: { ...def.sandbox, network: v as "enabled" | "disabled" } })} options={["disabled", "enabled"]} />
        </Field>
        <Field label="Allowed tools">
          <CheckList items={meta.tools} selected={def.sandbox.allowed_tools ?? []} onToggle={(t) => update({ sandbox: { ...def.sandbox, allowed_tools: toggleInList(def.sandbox.allowed_tools, t) } })} />
        </Field>
        <Field label="Granted secrets">
          {meta.secrets.length === 0 ? (
            <div className="text-xs text-zinc-500">No vaulted secrets — add them under Sandbox.</div>
          ) : (
            <CheckList items={meta.secrets} selected={def.sandbox.secret_refs ?? []} onToggle={(s) => update({ sandbox: { ...def.sandbox, secret_refs: toggleInList(def.sandbox.secret_refs, s) } })} />
          )}
        </Field>
        <Field label="Granted agents">
          <CheckList
            items={meta.agents.map((a) => a.provider)}
            selected={def.sandbox.agent_auth_refs ?? []}
            disabled={meta.agents.filter((a) => !a.runnable).map((a) => a.provider)}
            onToggle={(p) => update({ sandbox: { ...def.sandbox, agent_auth_refs: toggleInList(def.sandbox.agent_auth_refs, p) } })}
          />
        </Field>
      </Section>

      <Section title="Steps">
        <StepList steps={def.steps} onChange={setSteps} meta={meta} depth={0} />
      </Section>

      <Section title="Outputs (artifacts)">
        <OutputsEditor def={def} onChange={(outputs) => update({ outputs })} />
      </Section>

      {/* validation + save */}
      {validation && <ValidationPanel v={validation} />}
      <div className="flex flex-wrap gap-2">
        <Button variant="ghost" onClick={() => validate.mutate()}>
          Validate
        </Button>
        <Button onClick={() => save.mutate()} disabled={save.isPending}>
          {id ? "Save changes" : "Create"}
        </Button>
        <Button variant="ghost" onClick={() => setShowJson((s) => !s)}>
          {showJson ? "Hide JSON" : "Show JSON"}
        </Button>
      </div>
      {save.isError && <Card className="p-3 text-xs text-red-600">{(save.error as ApiError)?.message ?? "Save failed"}</Card>}

      {/* import / json preview */}
      <Section title="Import / JSON">
        <div className="space-y-2">
          <textarea
            className="h-28 w-full rounded-md border border-zinc-300 bg-white p-2 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-900"
            placeholder="Paste automation.v1 JSON to import…"
            value={importText}
            onChange={(e) => setImportText(e.target.value)}
          />
          <div className="flex gap-2">
            <Button variant="ghost" onClick={doImport} disabled={!importText.trim()}>
              <Upload size={15} /> Import
            </Button>
            {importErr && <span className="self-center text-xs text-red-600">{importErr}</span>}
          </div>
          {showJson && (
            <pre className="max-h-80 overflow-auto rounded-md bg-zinc-100 p-3 text-xs dark:bg-zinc-800">{JSON.stringify(def, null, 2)}</pre>
          )}
        </div>
      </Section>
    </div>
  );
}

function StepList({ steps, onChange, meta, depth }: { steps: AutomationStep[]; onChange: (s: AutomationStep[]) => void; meta: AutomationMeta; depth: number }) {
  const [adding, setAdding] = useState<AutomationStep["type"]>("tool");
  const siblingIds = steps.map((s) => s.id);
  return (
    <div className="space-y-2">
      {steps.map((step, i) => (
        <div key={i} className="rounded-md border border-zinc-200 p-2 dark:border-zinc-800">
          <div className="mb-2 flex items-center gap-2">
            <Input className="max-w-40" value={step.id} onChange={(e) => onChange(updateStepAt(steps, i, { id: e.target.value }))} />
            <span className="rounded bg-zinc-100 px-2 py-1 text-xs text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">{step.type}</span>
            <div className="ml-auto flex gap-1">
              <Button variant="ghost" onClick={() => onChange(moveStep(steps, i, i - 1))} disabled={i === 0}><ChevronUp size={14} /></Button>
              <Button variant="ghost" onClick={() => onChange(moveStep(steps, i, i + 1))} disabled={i === steps.length - 1}><ChevronDown size={14} /></Button>
              <Button variant="ghost" onClick={() => onChange(removeStepAt(steps, i))}><Trash2 size={14} /></Button>
            </div>
          </div>
          <StepFields step={step} siblingIds={siblingIds} meta={meta} depth={depth} onChange={(patch) => onChange(updateStepAt(steps, i, patch))} />
        </div>
      ))}
      {depth < 4 && (
        <div className="flex gap-2">
          <Select value={adding} onChange={(v) => setAdding(v as AutomationStep["type"])} options={STEP_TYPES} />
          <Button variant="ghost" onClick={() => onChange(addStep(steps, emptyStep(adding, uniqueStepId(steps, adding))))}>
            <Plus size={14} /> Add step
          </Button>
        </div>
      )}
    </div>
  );
}

function StepFields({ step, siblingIds, meta, depth, onChange }: { step: AutomationStep; siblingIds: string[]; meta: AutomationMeta; depth: number; onChange: (patch: Partial<AutomationStep>) => void }) {
  switch (step.type) {
    case "tool":
      return (
        <div className="space-y-2">
          <Field label="Tool">
            <Select value={step.tool ?? ""} onChange={(v) => onChange({ tool: v })} options={meta.tools} />
          </Field>
          <Field label="Arguments (JSON)">
            <JsonField value={step.with ?? {}} onChange={(obj) => onChange({ with: obj })} />
          </Field>
          <ErrorPolicy step={step} onChange={onChange} />
        </div>
      );
    case "condition":
      return (
        <div className="space-y-2">
          <Field label="Input (reference)">
            <Input value={step.if?.input ?? ""} onChange={(e) => onChange({ if: { ...step.if!, input: e.target.value } })} />
          </Field>
          <Field label="Operator">
            <Select value={step.if?.op ?? "is_empty"} onChange={(v) => onChange({ if: { ...step.if!, op: v } })} options={["is_empty", "is_not_empty", "exists", "not_exists", "eq", "ne", "contains", "gt", "lt"]} />
          </Field>
          <Field label="then (sibling ids, comma-separated)">
            <Input value={(step.then ?? []).join(",")} onChange={(e) => onChange({ then: splitCsv(e.target.value) })} />
          </Field>
          <Field label="else (sibling ids)">
            <Input value={(step.else ?? []).join(",")} onChange={(e) => onChange({ else: splitCsv(e.target.value) })} />
          </Field>
          <div className="text-xs text-zinc-500">Branch targets: {siblingIds.join(", ") || "(none)"}</div>
        </div>
      );
    case "for_each":
      return (
        <div className="space-y-2">
          <Field label="Items from (reference to an array)">
            <Input value={step.items_from ?? ""} onChange={(e) => onChange({ items_from: e.target.value })} />
          </Field>
          <div className="text-xs text-zinc-500">Binds <code>item</code> and <code>index</code> for child steps.</div>
          <StepList steps={step.steps ?? []} onChange={(steps) => onChange({ steps })} meta={meta} depth={depth + 1} />
          <ErrorPolicy step={step} onChange={onChange} />
        </div>
      );
    case "parallel":
      return (
        <div className="space-y-2">
          <StepList steps={step.steps ?? []} onChange={(steps) => onChange({ steps })} meta={meta} depth={depth + 1} />
          <ErrorPolicy step={step} onChange={onChange} />
        </div>
      );
    case "llm":
      return (
        <div className="space-y-2">
          <Field label="Prompt">
            <textarea className="h-20 w-full rounded-md border border-zinc-300 p-2 text-sm dark:border-zinc-700 dark:bg-zinc-900" value={step.prompt_template ?? ""} onChange={(e) => onChange({ prompt_template: e.target.value })} />
          </Field>
          <Field label="Inputs (references, comma-separated)">
            <Input value={(step.inputs ?? []).join(",")} onChange={(e) => onChange({ inputs: splitCsv(e.target.value) })} />
          </Field>
          <ErrorPolicy step={step} onChange={onChange} />
        </div>
      );
    case "agent_cli":
      return (
        <div className="space-y-2">
          <Field label="Adapter">
            <Select value={step.adapter ?? "codex"} onChange={(v) => onChange({ adapter: v })} options={meta.adapters} />
          </Field>
          <Field label="Prompt">
            <textarea className="h-20 w-full rounded-md border border-zinc-300 p-2 text-sm dark:border-zinc-700 dark:bg-zinc-900" value={step.prompt_template ?? ""} onChange={(e) => onChange({ prompt_template: e.target.value })} />
          </Field>
          <Field label="Workspace from (optional reference)">
            <Input value={step.workspace_from ?? ""} onChange={(e) => onChange({ workspace_from: e.target.value })} />
          </Field>
          <ErrorPolicy step={step} onChange={onChange} />
        </div>
      );
    case "finish":
      return (
        <div className="space-y-2">
          <Field label="Status">
            <Select value={step.status ?? "success"} onChange={(v) => onChange({ status: v })} options={["success", "skipped", "failed"]} />
          </Field>
          <Field label="Message">
            <Input value={step.message ?? ""} onChange={(e) => onChange({ message: e.target.value })} />
          </Field>
        </div>
      );
  }
}

function ErrorPolicy({ step, onChange }: { step: AutomationStep; onChange: (patch: Partial<AutomationStep>) => void }) {
  return (
    <label className="flex items-center gap-2 text-xs text-zinc-600 dark:text-zinc-300">
      <input type="checkbox" checked={!!step.continue_on_error} onChange={(e) => onChange({ continue_on_error: e.target.checked })} />
      Continue on error
    </label>
  );
}

function OutputsEditor({ def, onChange }: { def: AutomationDef; onChange: (o: AutomationDef["outputs"]) => void }) {
  const outs = def.outputs ?? [];
  const allIds: string[] = [];
  const walk = (steps: AutomationStep[]) => steps.forEach((s) => { allIds.push(s.id); if (s.steps) walk(s.steps); });
  walk(def.steps);
  return (
    <div className="space-y-2">
      {outs.map((o, i) => (
        <div key={i} className="flex flex-wrap items-center gap-2">
          <span className="text-xs text-zinc-500">artifact from</span>
          <Select value={o.from_step} onChange={(v) => onChange(outs.map((x, j) => (j === i ? { ...x, from_step: v } : x)))} options={allIds} />
          <Input className="max-w-48" placeholder="field (optional)" value={o.from_field ?? ""} onChange={(e) => onChange(outs.map((x, j) => (j === i ? { ...x, from_field: e.target.value } : x)))} />
          <Input className="max-w-48" placeholder="file name" value={o.name} onChange={(e) => onChange(outs.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))} />
          <Button variant="ghost" onClick={() => onChange(outs.filter((_, j) => j !== i))}><Trash2 size={14} /></Button>
        </div>
      ))}
      <Button variant="ghost" onClick={() => onChange([...outs, { type: "artifact", from_step: allIds[0] ?? "", name: "output.txt" }])}>
        <Plus size={14} /> Add artifact output
      </Button>
    </div>
  );
}

function ValidationPanel({ v }: { v: AutomationValidation }) {
  return (
    <Card className="space-y-1 p-3 text-xs">
      <div className={v.valid ? "text-emerald-600" : "text-red-600"}>{v.valid ? "Schema-valid ✓" : "Invalid — fix these:"}</div>
      {(v.issues ?? []).map((is, i) => (
        <div key={i} className="text-red-600">
          <code>{is.path}</code>: {is.message}
        </div>
      ))}
      {v.valid && !v.eligible && (
        <>
          <div className="pt-1 text-amber-600">Not yet runnable (review before enabling):</div>
          {(v.eligibility_issues ?? []).map((is, i) => (
            <div key={i} className="text-amber-600">
              <code>{is.path}</code>: {is.message}
            </div>
          ))}
        </>
      )}
    </Card>
  );
}

function Runs({ id, name, onBack }: { id: string; name: string; onBack: () => void }) {
  const runs = useQuery({ queryKey: ["automation-runs", id], queryFn: () => api.listAutomationRuns(id), refetchInterval: 4000 });
  const [open, setOpen] = useState<string | null>(null);
  const detail = useQuery({ queryKey: ["automation-run", open], queryFn: () => api.getAutomationRun(open!), enabled: !!open });

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <Button variant="ghost" onClick={onBack}><X size={15} /> Back</Button>
        <h1 className="text-lg font-semibold">Runs · {name}</h1>
      </div>
      {runs.isLoading ? (
        <Spinner />
      ) : (runs.data ?? []).length === 0 ? (
        <Card className="p-6 text-sm text-zinc-500">No runs yet.</Card>
      ) : (
        <div className="space-y-2">
          {(runs.data ?? []).map((r) => (
            <Card key={r.id} className="p-3">
              <button className="flex w-full items-center gap-3 text-left" onClick={() => setOpen(open === r.id ? null : r.id)}>
                <StatusBadge status={r.status} />
                <div className="text-sm">{r.trigger}</div>
                <div className="text-xs text-zinc-500">{r.started_at ? new Date(r.started_at).toLocaleString() : ""} · {r.duration_ms}ms</div>
                {r.error && <div className="ml-auto max-w-md truncate text-xs text-red-600">{r.error}</div>}
              </button>
              {open === r.id && detail.data && <RunDetail detail={detail.data} />}
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}

function RunDetail({ detail }: { detail: import("../lib/api.gen").AutomationRunDetail }) {
  const record = useMemo(() => {
    try {
      return detail.record as unknown as { steps?: unknown[]; model_calls?: number; agent_runs?: number; message?: string };
    } catch {
      return {};
    }
  }, [detail.record]);
  return (
    <div className="mt-2 space-y-2 border-t border-zinc-200 pt-2 text-xs dark:border-zinc-800">
      <div className="text-zinc-500">
        model calls {record.model_calls ?? 0} · agent runs {record.agent_runs ?? 0}
        {record.message ? ` · ${record.message}` : ""}
      </div>
      {detail.artifacts.length > 0 && (
        <div>
          <div className="font-medium">Artifacts</div>
          {detail.artifacts.map((a) => (
            <a key={a.id} className="block text-indigo-600 hover:underline" href={api.automationArtifactUrl(a.id)}>
              {a.name} ({a.size} bytes)
            </a>
          ))}
        </div>
      )}
      <pre className="max-h-72 overflow-auto rounded bg-zinc-100 p-2 dark:bg-zinc-800">{JSON.stringify(record, null, 2)}</pre>
    </div>
  );
}

// --- small primitives ---

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Card className="space-y-3 p-4">
      <div className="text-sm font-semibold">{title}</div>
      {children}
    </Card>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block space-y-1">
      <span className="text-xs font-medium text-zinc-500">{label}</span>
      {children}
    </label>
  );
}

function Select({ value, onChange, options }: { value: string; onChange: (v: string) => void; options: string[] }) {
  return (
    <select
      className="w-full rounded-md border border-zinc-300 bg-white px-2 py-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      {!options.includes(value) && <option value={value}>{value || "—"}</option>}
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </select>
  );
}

function NumberInput({ value, onChange }: { value: number; onChange: (n: number) => void }) {
  return <Input type="number" value={value} onChange={(e) => onChange(Number(e.target.value) || 0)} />;
}

function CheckList({ items, selected, onToggle, disabled }: { items: string[]; selected: string[]; onToggle: (v: string) => void; disabled?: string[] }) {
  if (items.length === 0) return <div className="text-xs text-zinc-500">(none)</div>;
  return (
    <div className="flex flex-wrap gap-2">
      {items.map((it) => {
        const isDisabled = disabled?.includes(it);
        return (
          <label key={it} className={`flex items-center gap-1 rounded border px-2 py-1 text-xs ${isDisabled ? "opacity-40" : ""} border-zinc-300 dark:border-zinc-700`} title={isDisabled ? "not currently runnable" : ""}>
            <input type="checkbox" checked={selected.includes(it)} disabled={isDisabled} onChange={() => onToggle(it)} />
            {it}
          </label>
        );
      })}
    </div>
  );
}

function JsonField({ value, onChange }: { value: Record<string, unknown>; onChange: (obj: Record<string, unknown>) => void }) {
  const [text, setText] = useState(JSON.stringify(value, null, 2));
  const [err, setErr] = useState("");
  return (
    <div className="space-y-1">
      <textarea
        className="h-24 w-full rounded-md border border-zinc-300 p-2 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-900"
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          try {
            onChange(JSON.parse(e.target.value));
            setErr("");
          } catch (ex) {
            setErr(ex instanceof Error ? ex.message : "invalid JSON");
          }
        }}
      />
      {err && <div className="text-xs text-red-600">{err}</div>}
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  const tone = statusTone(status);
  const cls = {
    ok: "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300",
    bad: "bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300",
    warn: "bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300",
    muted: "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300",
  }[tone];
  return <span className={`rounded px-2 py-0.5 text-xs font-medium ${cls}`}>{status}</span>;
}

function splitCsv(s: string): string[] {
  return s.split(",").map((x) => x.trim()).filter(Boolean);
}
