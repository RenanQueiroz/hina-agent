import { describe, expect, it } from "vitest";
import {
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
} from "./automations";

describe("automation defaults", () => {
  it("defaultDefinition is a disabled manual automation with one step", () => {
    const d = defaultDefinition();
    expect(d.schema_version).toBe("automation.v1");
    expect(d.enabled).toBe(false);
    expect(d.trigger.type).toBe("manual");
    expect(d.steps).toHaveLength(1);
  });

  it("prReviewTemplate matches the headline shape", () => {
    const d = prReviewTemplate();
    expect(d.trigger).toEqual({ type: "interval", every: "5m" });
    expect(d.sandbox.allowed_tools).toContain("github.notifications");
    expect(d.steps.map((s) => s.id)).toContain("review_each_pr");
    // The for_each carries nested steps (checkout -> parallel agents -> llm -> comment).
    const loop = d.steps.find((s) => s.id === "review_each_pr");
    expect(loop?.steps?.length).toBe(4);
    expect(d.outputs?.[0]?.name).toBe("final-review.md");
  });
});

describe("step list edits", () => {
  const base = [emptyStep("tool", "a"), emptyStep("finish", "b")];

  it("addStep appends", () => {
    expect(addStep(base, emptyStep("llm", "c"))).toHaveLength(3);
  });

  it("removeStepAt drops by index", () => {
    expect(removeStepAt(base, 0).map((s) => s.id)).toEqual(["b"]);
  });

  it("updateStepAt patches one step immutably", () => {
    const next = updateStepAt(base, 0, { tool: "github.notifications" });
    expect(next[0].tool).toBe("github.notifications");
    expect(base[0].tool).toBe("shell.exec"); // original untouched
  });

  it("moveStep reorders and is a no-op out of range", () => {
    expect(moveStep(base, 0, 1).map((s) => s.id)).toEqual(["b", "a"]);
    expect(moveStep(base, 0, 5)).toBe(base);
  });

  it("uniqueStepId avoids collisions including nested", () => {
    const steps = [
      { id: "x", type: "for_each" as const, steps: [{ id: "y", type: "finish" as const }] },
    ];
    expect(uniqueStepId(steps, "z")).toBe("z");
    expect(uniqueStepId(steps, "x")).toBe("x_2");
    expect(uniqueStepId(steps, "y")).toBe("y_2");
  });
});

describe("misc helpers", () => {
  it("toggleInList adds then removes", () => {
    expect(toggleInList(["a"], "b")).toEqual(["a", "b"]);
    expect(toggleInList(["a", "b"], "a")).toEqual(["b"]);
    expect(toggleInList(undefined, "a")).toEqual(["a"]);
  });

  it("parseDefinition rejects non-v1 + bad JSON", () => {
    expect(parseDefinition("{ not json").error).toBeTruthy();
    expect(parseDefinition('{"schema_version":"automation.v2"}').error).toBeTruthy();
    const ok = parseDefinition(JSON.stringify(defaultDefinition()));
    expect(ok.def?.name).toBe("New automation");
  });

  it("statusTone maps statuses", () => {
    expect(statusTone("success")).toBe("ok");
    expect(statusTone("failed")).toBe("bad");
    expect(statusTone("running")).toBe("warn");
  });
});
