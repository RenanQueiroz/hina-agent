package automation

// SchemaGuide returns a concise, authoritative description of the automation.v1
// document for the LLM-assisted creation flow (and as builder UI help text). It is
// deliberately compact — the backend JSON Schema (Validate) remains authoritative;
// this just teaches the model the shape so its draft is usually valid on the first
// try, with the validator's path-tagged errors driving any retries.
func SchemaGuide() string {
	return `You produce a JSON document of schema_version "automation.v1" — a scheduled workflow.
Output ONLY the JSON object, no prose, no code fences.

Top-level fields:
- schema_version: "automation.v1" (required)
- name: short title (required); description: optional
- enabled: always false (a human reviews before enabling)
- timezone: an IANA zone (e.g. "America/New_York") for cron; optional
- trigger: {type: "interval"|"cron"|"manual"}. interval -> {type,every:"5m"} (min 30s). cron -> {type,cron:"0 9 * * *"} (5 fields). manual -> {type:"manual"}.
- missed_run_policy: "skip" (default) or "run_once"
- concurrency: {policy: "skip_if_running"|"queue_one"|"parallel"|"cancel_previous", max_parallel: N}
- budget: {timeout_seconds, max_model_calls, max_agent_runs, max_tool_calls, max_log_bytes, max_artifact_bytes} (all optional; clamped to server caps)
- sandbox: the permission profile (the run is bound by THIS, not the user's chat policy):
    {mode: "granular"|"unrestricted", network: "enabled"|"disabled",
     allowed_tools: [tool names used], allowed_cli_tools: [...],
     allowed_host_services: ["llamacpp"], secret_refs: [vaulted secret names],
     agent_auth_refs: [granted agent names like "codex"],
     resources: {cpus, memory_mb, pids}}
- steps: ordered list (required). Each step has a unique id ([a-zA-Z_][a-zA-Z0-9_]*; not "item"/"index") and a type:
    tool: {id,type:"tool",tool:<name>,with:{...args}}
    condition: {id,type:"condition",if:{input:<ref>,op:"is_empty"|"is_not_empty"|"eq"|"ne"|"contains"|"gt"|"lt"|"exists"|"not_exists",value?},then:[stepIds],else:[stepIds]}
    for_each: {id,type:"for_each",items_from:<ref to an array>,steps:[...]}  (binds item, index)
    parallel: {id,type:"parallel",steps:[...]}  (runs children concurrently; output is a map of child outputs)
    llm: {id,type:"llm",prompt_template:"...",inputs:[refs],output_schema?:{...}}
    agent_cli: {id,type:"agent_cli",adapter:"codex"|"claude"|"cursor"|"pi",prompt_template:"...",workspace_from?:<ref>,model?,max_turns?,output_schema?}
    finish: {id,type:"finish",status:"success"|"skipped"|"failed",message?}
  Any step may set continue_on_error:true (default false fails the run).
- outputs: [{type:"artifact",from_step:<stepId>,from_field?:<field>,name:"file.md"}]

Deterministic tools (use only these): github.notifications, github.pr_checkout, github.pr_comment, http.request, shell.exec. (mcp.call is not available yet.)

References: a value from a prior step is "<stepId>.<path>" (e.g. find.items, find.items[0].pr). Inside a for_each, "item" and "index" are bound (item.pr). Free-text fields use ${...} templates (e.g. "Review PR ${item.pr}"); a value that is exactly "${ref}" keeps the referenced value's type. A tool arg key ending in "_from" (e.g. body_from) takes a bare reference.

Condition steps list their then/else branch step ids among the SAME-level siblings; those branch steps are not run in sequence — only when their branch fires. Put cheap deterministic checks first so a run can finish (e.g. a "finish" with status "skipped") without waking any model or agent.`
}
