import { describe, expect, it } from "vitest";

import type { ObjectChange, PlanDoc } from "../ipc/types";
import { computeAutoExcluded, dependenciesOf, overlayPlan, runningChanges } from "./plan";

const change = (id: string, kind: ObjectChange["changeKind"], dependsOn: string[] = [], statements = 1): ObjectChange => ({
  id,
  objectType: "table",
  name: id,
  changeKind: kind,
  dependsOn,
  statements: Array.from({ length: statements }, (_, i) => ({ sql: `-- ${id} ${i}`, transactional: true })),
});

const plan = (changes: ObjectChange[]): PlanDoc => ({
  formatVersion: 2,
  statementCount: changes.reduce((n, c) => n + c.statements.length, 0),
  dataLoss: false,
  changes,
});

// a ← b ← c (c depends on b, b on a); d is unrelated; x is a drop that depends on a.
const base = plan([change("a", "add"), change("b", "add", ["a", "schema:outside"]), change("c", "alter", ["b"]), change("d", "add"), change("x", "drop", ["a"])]);

describe("computeAutoExcluded", () => {
  it("gates every change that transitively depends on an excluded one, naming the cause", () => {
    const auto = computeAutoExcluded(base, new Set(["a"]));
    expect([...auto]).toEqual([
      ["b", "a"],
      ["c", "b"],
    ]);
  });

  it("never gates a drop, and ignores dependencies outside the plan", () => {
    expect(computeAutoExcluded(base, new Set(["a"])).has("x")).toBe(false);
    expect(computeAutoExcluded(base, new Set(["schema:outside"])).size).toBe(0);
  });

  it("is empty without a plan or an exclusion", () => {
    expect(computeAutoExcluded(undefined, new Set(["a"])).size).toBe(0);
    expect(computeAutoExcluded(base, new Set()).size).toBe(0);
  });
});

describe("dependenciesOf", () => {
  it("returns the transitive dependencies within the plan only", () => {
    expect([...dependenciesOf(base, "c")].sort()).toEqual(["a", "b"]);
    expect(dependenciesOf(base, "d").size).toBe(0);
  });
});

describe("overlayPlan", () => {
  it("keeps the comparison's list and order, takes what runs from the re-plan", () => {
    const effective = plan([change("c", "alter", ["b"], 3), change("a", "add")]);
    effective.statementCount = 4;
    const out = overlayPlan(base, effective);
    expect(out.changes.map((c) => c.id)).toEqual(["a", "b", "c", "d", "x"]);
    expect(out.statementCount).toBe(4);
    expect(out.changes.find((c) => c.id === "c")?.statements).toHaveLength(3);
    const b = out.changes.find((c) => c.id === "b");
    expect(b?.statements).toEqual([]);
    expect(b?.dependsOn).toEqual(["a", "schema:outside"]);
    expect(runningChanges(out).map((c) => c.id)).toEqual(["a", "c"]);
  });

  it("appends an object only the re-plan has (a dependent the engine adds)", () => {
    const out = overlayPlan(base, plan([change("a", "add"), change("v", "alter", ["a"])]));
    expect(out.changes.map((c) => c.id)).toEqual(["a", "b", "c", "d", "x", "v"]);
  });
});
