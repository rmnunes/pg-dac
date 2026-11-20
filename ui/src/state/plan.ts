// Pure helpers over a plan and the user's selection: dependency gating, the
// overlay of a re-planned migration on the full comparison, and the signature
// that tells a stale plan from a current one. No store, no I/O — unit-tested
// on their own.

import type { ObjectChange, PlanDoc } from "../ipc/types";

// computeAutoExcluded derives the dependency-gated exclusions: a non-drop
// change whose dependsOn names an excluded change (manually or in turn
// automatically) cannot be applied on its own, so it is excluded too — the
// SSDT Schema Compare behavior. Dependencies that are not part of the plan are
// already present in the Target and never gate anything.
export function computeAutoExcluded(plan: PlanDoc | undefined, manual: Set<string>): Map<string, string> {
  const auto = new Map<string, string>();
  if (!plan) return auto;
  const inPlan = new Set(plan.changes.map((c) => c.id));
  let changed = true;
  while (changed) {
    changed = false;
    for (const c of plan.changes) {
      if (c.changeKind === "drop" || manual.has(c.id) || auto.has(c.id)) continue;
      const cause = (c.dependsOn ?? []).find((d) => inPlan.has(d) && (manual.has(d) || auto.has(d)));
      if (cause) {
        auto.set(c.id, cause);
        changed = true;
      }
    }
  }
  return auto;
}

// dependenciesOf returns the transitive plan dependencies of a change (ids in
// the plan only).
export function dependenciesOf(plan: PlanDoc, id: string): Set<string> {
  const byId = new Map(plan.changes.map((c) => [c.id, c]));
  const out = new Set<string>();
  const stack = [id];
  while (stack.length) {
    const cur = byId.get(stack.pop()!);
    for (const d of cur?.dependsOn ?? []) {
      if (byId.has(d) && !out.has(d)) {
        out.add(d);
        stack.push(d);
      }
    }
  }
  return out;
}

// overlayPlan keeps the full comparison's object list (so excluded objects stay
// visible and re-includable) and takes everything that will actually run —
// statements, hazards, reasons, counts — from the re-planned migration. An
// object the migration no longer touches keeps its definitions and carries no
// statements.
export function overlayPlan(base: PlanDoc, effective: PlanDoc): PlanDoc {
  const byId = new Map(effective.changes.map((c) => [c.id, c]));
  const changes: ObjectChange[] = base.changes.map((b) => byId.get(b.id) ?? { ...b, statements: [], hazards: [], severity: "", reason: "" });
  const known = new Set(base.changes.map((b) => b.id));
  for (const e of effective.changes) if (!known.has(e.id)) changes.push(e);
  return { ...effective, changes };
}

// Selection helpers shared by the list, the detail pane and the publish dialog.
export const isExcluded = (id: string, excluded: Set<string>, autoExcluded: Map<string, string>): boolean =>
  excluded.has(id) || autoExcluded.has(id);

/** The changes that run under the current selection: the re-planned migration left them statements. */
export const runningChanges = (plan: PlanDoc | undefined): ObjectChange[] => (plan?.changes ?? []).filter((c) => c.statements.length > 0);
