import { create } from "zustand";

import * as api from "../ipc/client";
import type { CompareEvent, CompareRequest, ObjectChange, PlanDoc, Profile, ProfileKind, PublishEvent } from "../ipc/types";
import { profileRef, resolveSpec } from "./profiles";

type Status = "idle" | "comparing" | "scripting" | "error";

interface PublishState {
  running: boolean;
  total: number;
  applied: number;
  log: string[];
  done: boolean;
  error?: string;
}

// Live progress of an in-flight compare, shown in the CompareProgress dialog.
interface CompareProgress {
  running: boolean;
  opId?: string;
  from: string; // echo of the sources being compared, for the dialog header
  to: string;
  message: string; // current phase message
  steps: string[]; // completed phase messages (a small log)
  fromCount?: number;
  toCount?: number;
  canceled?: boolean;
}

interface State {
  // sources
  from: string;
  to: string;
  // options
  schemas: string; // comma-separated
  targetMajor: string; // "" or number
  online: boolean;
  allowHazards: boolean;
  dropObjects: boolean;
  requireKey: string;
  skipDeployScripts: boolean;
  allowDrift: boolean;

  // results
  status: Status;
  error?: string;
  plan?: PlanDoc;
  warnings: string[];
  // Objects the user unchecked.
  excluded: Set<string>;
  // Objects excluded because something they depend on is excluded (SSDT-style
  // dependency gating): id -> the excluded dependency that caused it.
  autoExcluded: Map<string, string>;
  selectedId?: string;

  publish: PublishState;
  compareProgress: CompareProgress;

  // saved source profiles
  profiles: Profile[];

  // UI layout (persisted to localStorage)
  treeWidth: number;

  // actions
  set: (patch: Partial<State>) => void;
  setTreeWidth: (n: number) => void;
  persistTreeWidth: () => void;
  swap: () => void;
  request: () => CompareRequest;
  loadProfiles: () => Promise<void>;
  addProfile: (p: { name: string; kind: ProfileKind; value: string }) => Promise<void>;
  removeProfile: (id: string) => Promise<void>;
  runCompare: () => Promise<void>;
  cancelCompare: () => void;
  handleCompareEvent: (e: CompareEvent) => void;
  generateScript: () => Promise<string>;
  toggleExclude: (id: string) => void;
  select: (id: string) => void;
  startPublish: () => Promise<void>;
  handlePublishEvent: (e: PublishEvent) => void;
}

const splitSchemas = (s: string): string[] =>
  s.split(",").map((x) => x.trim()).filter(Boolean);

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
function dependenciesOf(plan: PlanDoc, id: string): Set<string> {
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

// Persisted change-list width (the draggable divider between the change list and
// the results). Kept in localStorage so it survives restarts.
const TREE_WIDTH_KEY = "pgdac.treeWidth";
const clampWidth = (n: number): number => Math.max(240, Math.min(900, Math.round(n)));
function loadTreeWidth(): number {
  const v = Number(localStorage.getItem(TREE_WIDTH_KEY));
  return Number.isFinite(v) && v > 0 ? clampWidth(v) : 420;
}

export const useStore = create<State>((set, get) => ({
  from: "",
  to: "",
  schemas: "",
  targetMajor: "",
  online: true,
  allowHazards: false,
  dropObjects: false,
  requireKey: "",
  skipDeployScripts: false,
  allowDrift: false,

  status: "idle",
  warnings: [],
  excluded: new Set<string>(),
  autoExcluded: new Map<string, string>(),

  publish: { running: false, total: 0, applied: 0, log: [], done: false },
  compareProgress: { running: false, from: "", to: "", message: "", steps: [] },
  profiles: [],
  treeWidth: loadTreeWidth(),

  set: (patch) => set(patch),
  // Live during a drag (cheap); persisted once on mouse-up via persistTreeWidth.
  setTreeWidth: (n) => set({ treeWidth: clampWidth(n) }),
  persistTreeWidth: () => {
    try {
      localStorage.setItem(TREE_WIDTH_KEY, String(get().treeWidth));
    } catch {
      /* storage unavailable — width just won't persist */
    }
  },

  swap: () => set((s) => ({ from: s.to, to: s.from })),

  loadProfiles: async () => {
    try {
      set({ profiles: await api.listProfiles() });
    } catch (e) {
      set({ error: String(e) });
    }
  },

  addProfile: async ({ name, kind, value }) => {
    const profile: Profile = { id: crypto.randomUUID(), name, kind, value };
    try {
      set({ profiles: await api.addProfile(profile) });
    } catch (e) {
      set({ error: String(e) });
    }
  },

  removeProfile: async (id) => {
    try {
      const profiles = await api.removeProfile(id);
      // If either side referenced the deleted profile, clear that side.
      set((s) => ({
        profiles,
        from: s.from === profileRef(id) ? "" : s.from,
        to: s.to === profileRef(id) ? "" : s.to,
      }));
    } catch (e) {
      set({ error: String(e) });
    }
  },

  request: () => {
    const s = get();
    const schemas = splitSchemas(s.schemas);
    const major = parseInt(s.targetMajor, 10);
    const excludeIds = [...new Set([...s.excluded, ...s.autoExcluded.keys()])];
    return {
      // Resolve profile refs to the real value the daemon needs (the secret
      // travels only here, at send time — it's never bound to a visible input).
      from: resolveSpec(s.from, s.profiles),
      to: resolveSpec(s.to, s.profiles),
      schemas: schemas.length ? schemas : undefined,
      targetMajor: Number.isFinite(major) && major > 0 ? major : undefined,
      online: s.online,
      allowHazards: s.allowHazards,
      dropObjectsNotInSource: s.dropObjects,
      excludeIds: excludeIds.length ? excludeIds : undefined,
      requireKey: s.requireKey.trim() || undefined,
      skipDeployScripts: s.skipDeployScripts || undefined,
      allowDrift: s.allowDrift || undefined,
    };
  },

  runCompare: async () => {
    const s = get();
    const opId = crypto.randomUUID();
    set({
      status: "comparing",
      error: undefined,
      plan: undefined,
      warnings: [],
      excluded: new Set(),
      autoExcluded: new Map(),
      selectedId: undefined,
      compareProgress: {
        running: true,
        opId,
        from: s.from,
        to: s.to,
        message: "Starting…",
        steps: [],
        canceled: false,
      },
    });
    try {
      // Fires and returns immediately; progress/result arrive via handleCompareEvent.
      await api.startCompare({ ...s.request(), opId });
    } catch (e) {
      set((st) => ({
        status: "error",
        error: String(e),
        compareProgress: { ...st.compareProgress, running: false },
      }));
    }
  },

  cancelCompare: () => {
    const { opId } = get().compareProgress;
    set((s) => ({ compareProgress: { ...s.compareProgress, message: "Canceling…" } }));
    if (opId) void api.cancelCompare(opId);
  },

  handleCompareEvent: (e) =>
    set((s) => {
      const p = { ...s.compareProgress };
      // Once a compare is no longer running (canceled or errored), ignore any
      // late phase/result events from the daemon for the previous run.
      if ((e.event === "phase" || e.event === "result") && !p.running) {
        return {};
      }
      switch (e.event) {
        case "phase":
          if (p.message) p.steps = [...p.steps, p.message];
          p.message = e.data.message ?? p.message;
          if (e.data.fromCount !== undefined) p.fromCount = e.data.fromCount;
          if (e.data.toCount !== undefined) p.toCount = e.data.toCount;
          return { compareProgress: p };
        case "result": {
          const plan = e.data.plan;
          p.running = false;
          return {
            compareProgress: p,
            status: "idle",
            plan,
            warnings: e.data.warnings ?? [],
            selectedId: plan?.changes[0]?.id,
          };
        }
        case "canceled":
          p.running = false;
          p.canceled = true;
          p.message = "Canceled.";
          return { compareProgress: p, status: "idle" };
        case "error":
          p.running = false;
          return { compareProgress: p, status: "error", error: e.data.message ?? "compare failed" };
        default:
          return { compareProgress: p };
      }
    }),

  generateScript: async () => {
    set({ status: "scripting", error: undefined });
    try {
      const res = await api.script(get().request());
      set({ status: "idle", warnings: res.warnings ?? get().warnings });
      return res.sql;
    } catch (e) {
      set({ status: "error", error: String(e) });
      return "";
    }
  },

  // Excluding an object also excludes everything in the plan that depends on
  // it; including one brings its plan dependencies back (SSDT behavior).
  toggleExclude: (id) =>
    set((s) => {
      const next = new Set(s.excluded);
      if (next.has(id) || s.autoExcluded.has(id)) {
        next.delete(id);
        if (s.plan) for (const dep of dependenciesOf(s.plan, id)) next.delete(dep);
      } else {
        next.add(id);
      }
      return { excluded: next, autoExcluded: computeAutoExcluded(s.plan, next) };
    }),

  select: (id) => set({ selectedId: id }),

  startPublish: async () => {
    set({ publish: { running: true, total: 0, applied: 0, log: [], done: false, error: undefined } });
    try {
      await api.startPublish(get().request());
    } catch (e) {
      set((s) => ({ publish: { ...s.publish, running: false, error: String(e) } }));
    }
  },

  handlePublishEvent: (e) =>
    set((s) => {
      const p = { ...s.publish };
      switch (e.event) {
        case "start":
          p.total = e.data.statementCount ?? 0;
          p.log = [...p.log, `start: ${p.total} statement(s)${e.data.dataLoss ? " (DATA LOSS)" : ""}`];
          break;
        case "progress":
          p.applied = e.data.index ?? p.applied;
          p.total = e.data.total ?? p.total;
          if (e.data.sql) p.log = [...p.log, `[${e.data.index}/${e.data.total}] ${firstLine(e.data.sql)}`];
          break;
        case "done":
          p.running = false;
          p.done = true;
          p.applied = e.data.applied ?? p.applied;
          p.log = [...p.log, `done: applied ${p.applied} statement(s)`];
          break;
        case "error":
          p.running = false;
          p.error = e.data.message ?? "publish failed";
          p.log = [...p.log, `ERROR: ${p.error}`];
          break;
      }
      return { publish: p };
    }),
}));

function firstLine(sql: string): string {
  const i = sql.indexOf("\n");
  return i >= 0 ? sql.slice(0, i) + " …" : sql;
}

// Selectors / helpers used by components.
export const includedChanges = (plan: PlanDoc | undefined, excluded: Set<string>): ObjectChange[] =>
  (plan?.changes ?? []).filter((c) => !excluded.has(c.id));
