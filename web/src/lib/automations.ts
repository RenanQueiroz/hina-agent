// Pure helpers for the Automations builder, factored out so the form logic is
// unit-tested under the node-env vitest (no DOM). The server's POST /validate is
// authoritative for validation; these helpers shape + edit the automation.v1
// document the on-rails form binds to.

// A loose, editor-friendly view of an automation.v1 document. The server schema is
// authoritative; this is just enough typing for the form controls.
export interface AutomationStep {
  id: string;
  type: "tool" | "condition" | "for_each" | "parallel" | "llm" | "agent_cli" | "finish";
  tool?: string;
  with?: Record<string, unknown>;
  if?: { input: string; op: string; value?: unknown };
  then?: string[];
  else?: string[];
  items_from?: string;
  steps?: AutomationStep[];
  inputs?: string[];
  prompt_template?: string;
  output_schema_ref?: string;
  adapter?: string;
  workspace_from?: string;
  model?: string;
  max_turns?: number;
  status?: string;
  message?: string;
  continue_on_error?: boolean;
}

export interface AutomationOutput {
  type: "artifact";
  from_step: string;
  from_field?: string;
  name: string;
}

export interface AutomationDef {
  schema_version: "automation.v1";
  name: string;
  description?: string;
  enabled: boolean;
  timezone?: string;
  trigger: { type: "interval" | "cron" | "manual"; every?: string; cron?: string };
  missed_run_policy?: "skip" | "run_once";
  concurrency: { policy: string; max_parallel?: number };
  budget: {
    timeout_seconds?: number;
    max_model_calls?: number;
    max_agent_runs?: number;
    max_tool_calls?: number;
    max_log_bytes?: number;
    max_artifact_bytes?: number;
  };
  sandbox: {
    mode: "granular" | "unrestricted";
    network?: "enabled" | "disabled";
    allowed_host_services?: string[];
    allowed_cli_tools?: string[];
    allowed_tools?: string[];
    secret_refs?: string[];
    agent_auth_refs?: string[];
    resources?: { cpus?: number; memory_mb?: number; pids?: number };
  };
  steps: AutomationStep[];
  outputs?: AutomationOutput[];
  schemas?: Record<string, unknown>;
}

export const STEP_TYPES: AutomationStep["type"][] = [
  "tool",
  "condition",
  "for_each",
  "parallel",
  "llm",
  "agent_cli",
  "finish",
];

// defaultDefinition is a minimal, valid starting point: a manual trigger with a
// single finish step, disabled (a human reviews + enables).
export function defaultDefinition(): AutomationDef {
  return {
    schema_version: "automation.v1",
    name: "New automation",
    description: "",
    enabled: false,
    trigger: { type: "manual" },
    missed_run_policy: "skip",
    concurrency: { policy: "skip_if_running", max_parallel: 1 },
    budget: {
      timeout_seconds: 600,
      max_model_calls: 8,
      max_agent_runs: 4,
      max_log_bytes: 1048576,
      max_artifact_bytes: 5242880,
    },
    sandbox: { mode: "granular", network: "disabled", allowed_tools: [] },
    steps: [{ id: "done", type: "finish", status: "success", message: "Done." }],
    outputs: [],
  };
}

// prReviewTemplate is the headline GitHub PR-review automation, so a user can build
// it with one click and then tailor it.
export function prReviewTemplate(): AutomationDef {
  return {
    schema_version: "automation.v1",
    name: "GitHub PR review requests",
    description: "Check for requested PR reviews and draft a combined review.",
    enabled: false,
    timezone: "America/New_York",
    trigger: { type: "interval", every: "5m" },
    missed_run_policy: "skip",
    concurrency: { policy: "skip_if_running", max_parallel: 1 },
    budget: {
      timeout_seconds: 1800,
      max_model_calls: 12,
      max_agent_runs: 4,
      max_log_bytes: 10485760,
      max_artifact_bytes: 52428800,
    },
    sandbox: {
      mode: "granular",
      network: "enabled",
      allowed_host_services: ["llamacpp"],
      allowed_cli_tools: ["sh", "git", "gh", "codex", "claude"],
      allowed_tools: ["github.notifications", "github.pr_checkout", "github.pr_comment"],
      secret_refs: ["github_token"],
      agent_auth_refs: ["codex", "claude"],
      resources: { cpus: 4, memory_mb: 8192, pids: 512 },
    },
    steps: [
      {
        id: "find_review_requests",
        type: "tool",
        tool: "github.notifications",
        with: { reasons: ["review_requested"], include_participating: true },
      },
      {
        id: "skip_if_empty",
        type: "condition",
        if: { input: "find_review_requests.items", op: "is_empty" },
        then: ["finish_noop"],
        else: ["review_each_pr"],
      },
      { id: "finish_noop", type: "finish", status: "skipped", message: "No matching review requests." },
      {
        id: "review_each_pr",
        type: "for_each",
        items_from: "find_review_requests.items",
        steps: [
          {
            id: "checkout_pr",
            type: "tool",
            tool: "github.pr_checkout",
            with: { notification: "${item}" },
          },
          {
            id: "agent_reviews",
            type: "parallel",
            steps: [
              {
                id: "codex_review",
                type: "agent_cli",
                adapter: "codex",
                workspace_from: "checkout_pr.workspace",
                prompt_template: "Review this PR for correctness, regressions, security, and missing tests.",
              },
              {
                id: "claude_review",
                type: "agent_cli",
                adapter: "claude",
                workspace_from: "checkout_pr.workspace",
                prompt_template: "Review this PR for correctness, regressions, security, and missing tests.",
              },
            ],
          },
          {
            id: "combine_reviews",
            type: "llm",
            inputs: ["agent_reviews"],
            prompt_template: "Merge the review reports, remove duplicates, verify claims, and produce a final PR review.",
          },
          {
            id: "post_review",
            type: "tool",
            tool: "github.pr_comment",
            with: { pr: "${item.pr}", repo: "${item.repository}", body_from: "combine_reviews.markdown" },
          },
        ],
      },
    ],
    outputs: [{ type: "artifact", from_step: "combine_reviews", name: "final-review.md" }],
  };
}

// emptyStep builds a new step of a type with sensible defaults.
export function emptyStep(type: AutomationStep["type"], id: string): AutomationStep {
  switch (type) {
    case "tool":
      return { id, type, tool: "shell.exec", with: {} };
    case "condition":
      return { id, type, if: { input: "", op: "is_empty" }, then: [], else: [] };
    case "for_each":
      return { id, type, items_from: "", steps: [] };
    case "parallel":
      return { id, type, steps: [] };
    case "llm":
      return { id, type, prompt_template: "", inputs: [] };
    case "agent_cli":
      return { id, type, adapter: "codex", prompt_template: "" };
    case "finish":
      return { id, type, status: "success", message: "" };
  }
}

// --- immutable list edits (unit-tested) ---

export function updateStepAt(steps: AutomationStep[], index: number, patch: Partial<AutomationStep>): AutomationStep[] {
  return steps.map((s, i) => (i === index ? { ...s, ...patch } : s));
}

export function removeStepAt(steps: AutomationStep[], index: number): AutomationStep[] {
  return steps.filter((_, i) => i !== index);
}

export function moveStep(steps: AutomationStep[], from: number, to: number): AutomationStep[] {
  if (to < 0 || to >= steps.length || from === to) return steps;
  const next = steps.slice();
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved);
  return next;
}

export function addStep(steps: AutomationStep[], step: AutomationStep): AutomationStep[] {
  return [...steps, step];
}

// uniqueStepId returns an id not already used among the given steps (recursively),
// so a new step never collides.
export function uniqueStepId(steps: AutomationStep[], base: string): string {
  const used = new Set<string>();
  const walk = (list: AutomationStep[]) => {
    for (const s of list) {
      used.add(s.id);
      if (s.steps) walk(s.steps);
    }
  };
  walk(steps);
  if (!used.has(base)) return base;
  for (let i = 2; ; i++) {
    const candidate = `${base}_${i}`;
    if (!used.has(candidate)) return candidate;
  }
}

// toggleInList adds/removes a value from a string list, preserving order.
export function toggleInList(list: string[] | undefined, value: string): string[] {
  const cur = list ?? [];
  return cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value];
}

// parseDefinition safely parses imported JSON, returning an error message on failure.
export function parseDefinition(text: string): { def?: AutomationDef; error?: string } {
  try {
    const obj = JSON.parse(text);
    if (typeof obj !== "object" || obj === null) return { error: "not a JSON object" };
    if (obj.schema_version !== "automation.v1") return { error: 'schema_version must be "automation.v1"' };
    return { def: obj as AutomationDef };
  } catch (e) {
    return { error: e instanceof Error ? e.message : "invalid JSON" };
  }
}

// statusTone maps a run status to a small palette key for the UI.
export function statusTone(status: string): "ok" | "warn" | "bad" | "muted" {
  switch (status) {
    case "success":
      return "ok";
    case "skipped":
      return "muted";
    case "failed":
      return "bad";
    case "cancelled":
      return "warn";
    case "running":
      return "warn";
    default:
      return "muted";
  }
}
