import { create } from "zustand";

import * as api from "../ipc/client";
import type {
  ColumnOrderMode,
  CommentsMode,
  CompareEvent,
  CompareRequest,
  ObjectChange,
  PlanDoc,
  PrivilegesMode,
  Profile,
  ProfileKind,
  PublishEvent,
  Warning,
  WhitespaceMode,
} from "../ipc/types";
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

// The comparison and deployment options (the Options dialog). "" on a
// comparison-semantics option defers to the Source project's pgdac.yaml.
export interface OptionsState {
  schemas: string; // comma-separated
  targetMajor: string; // "" or number
  online: boolean;
  dropObjects: boolean;
  requireKey: string;
  skipDeployScripts: boolean;
  allowDrift: boolean;
  privileges: PrivilegesMode;
  columnOrder: ColumnOrderMode;
  comments: CommentsMode;
  whitespace: WhitespaceMode;
  excludeTypes: string; // comma-separated object types left out of the comparison
  variables: string; // name=value per line, for $(name) in deploy scripts
  singleTransaction: boolean;
}

export const OPTION_DEFAULTS: OptionsState = {
  schemas: "",
  targetMajor: "",
  online: true,
  dropObjects: false,
  requireKey: "",
  skipDeployScripts: false,
  allowDrift: false,
  privileges: "",
  columnOrder: "",
  comments: "",
  whitespace: "",
  excludeTypes: "",
  variables: "",
  singleTransaction: false,
};

const OPTION_KEYS = Object.keys(OPTION_DEFAULTS) as (keyof OptionsState)[];

/** How many options differ from their defaults (the toolbar badge). */
export const changedOptionCount = (s: OptionsState): number => OPTION_KEYS.filter((k) => s[k] !== OPTION_DEFAULTS[k]).length;

interface State extends OptionsState {
  // sources
  from: string;
  to: string;
  // Set when the user acknowledges the error-severity hazards in the publish
  // dialog; the engine refuses to apply them otherwise.
  allowHazards: boolean;

  // results
  status: Status;
  error?: string;
  // plan is what will run under the current selection (re-planned by the daemon
  // on every include/exclude change); basePlan is the full comparison it was
  // derived from, which keeps excluded objects listed so they can be re-included.
  plan?: PlanDoc;
  basePlan?: PlanDoc;
  replanning: boolean;
  warnings: Warning[];
  comparedAt?: number;
  // The sources and options the current plan was computed with, so the UI can
  // say when the options changed since.
  comparedSignature?: string;
  // Objects the user unchecked.
  excluded: Set<string>;
  // Objects excluded because something they depend on is excluded (SSDT-style
  // dependency gating): id -> the excluded dependency that caused it.
  autoExcluded: Map<string, string>;
  selectedId?: string;
  // What the last include/exclude toggle did to other rows (dependents gated,
  // dependencies brought back), until dismissed or the next toggle.
  gatingNotice?: { text: string };

  publish: PublishState;
  compareProgress: CompareProgress;

  // saved source profiles
  profiles: Profile[];

  // UI layout (persisted to localStorage)
  treeWidth: number;

  // actions
  set: (patch: Partial<State>) => void;
  resetOptions: () => void;
  setTreeWidth: (n: number) => void;
  persistTreeWidth: () => void;
  swap: () => void;
  request: () => CompareRequest;
  loadProfiles: () => Promise<void>;
  addProfile: (p: { name: string; kind: ProfileKind; value: string }) => Promise<Profile | undefined>;
  removeProfile: (id: string) => Promise<void>;
  runCompare: () => Promise<void>;
  cancelCompare: () => void;
  handleCompareEvent: (e: CompareEvent) => void;
  generateScript: () => Promise<string>;
  toggleExclude: (id: string) => void;
  setIncluded: (ids: string[], included: boolean) => void;
  replan: () => void;
  clearGatingNotice: () => void;
  select: (id: string) => void;
  dismissWarning: (index: number) => void;
  startPublish: () => Promise<void>;
  handlePublishEvent: (e: PublishEvent) => void;
}

const splitSchemas = (s: string): string[] =>
  s.split(",").map((x) => x.trim()).filter(Boolean);

// splitList → a comma-separated option as a list, or undefined when empty so
// the daemon falls back to the project's default.
const splitList = (s: string): string[] | undefined => {
  const items = splitSchemas(s);
  return items.length ? items : undefined;
};

// parseVariables → "name=value" lines as a map (blank lines and lines without
// "=" are ignored), or undefined when nothing was entered.
const parseVariables = (s: string): Record<string, string> | undefined => {
  const out: Record<string, string> = {};
  let any = false;
  for (const line of s.split("\n")) {
    const i = line.indexOf("=");
    if (i <= 0) continue;
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    any = true;
  }
  return any ? out : undefined;
};

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
export const TREE_WIDTH_MIN = 240;
export const TREE_WIDTH_MAX = 900;
const clampWidth = (n: number): number => Math.max(TREE_WIDTH_MIN, Math.min(TREE_WIDTH_MAX, Math.round(n)));
function loadTreeWidth(): number {
  try {
    const v = Number(localStorage.getItem(TREE_WIDTH_KEY));
    return Number.isFinite(v) && v > 0 ? clampWidth(v) : 420;
  } catch {
    return 420;
  }
}

// What a plan depends on besides the object selection: the two sources and
// every option. Compared against the stored signature to flag a stale plan.
export function optionsSignature(s: OptionsState & { from: string; to: string }): string {
  const o: Record<string, unknown> = { from: s.from, to: s.to };
  for (const k of OPTION_KEYS) o[k] = s[k];
  return JSON.stringify(o);
}

export const optionsStale = (s: State): boolean => !!s.plan && s.comparedSignature !== optionsSignature(s);

export const useStore = create<State>((set, get) => ({
  from: "",
  to: "",
  ...OPTION_DEFAULTS,
  allowHazards: false,

  status: "idle",
  replanning: false,
  warnings: [],
  excluded: new Set<string>(),
  autoExcluded: new Map<string, string>(),

  publish: { running: false, total: 0, applied: 0, log: [], done: false },
  compareProgress: { running: false, from: "", to: "", message: "", steps: [] },
  profiles: [],
  treeWidth: loadTreeWidth(),

  set: (patch) => set(patch),
  resetOptions: () => set({ ...OPTION_DEFAULTS }),
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
      const profiles = await api.addProfile(profile);
      set({ profiles });
      return profiles.find((p) => p.id === profile.id) ?? profiles.find((p) => p.value === value && p.kind === kind);
    } catch (e) {
      set({ error: String(e) });
      return undefined;
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
      // Only an explicit choice is sent; "" leaves the project's default in force.
      ignorePrivileges: s.privileges === "" ? undefined : s.privileges === "ignore",
      columnOrder: s.columnOrder || undefined,
      ignoreComments: s.comments === "" ? undefined : s.comments === "ignore",
      compareWhitespace: s.whitespace === "" ? undefined : s.whitespace === "compare",
      excludeObjectTypes: splitList(s.excludeTypes),
      variables: parseVariables(s.variables),
      singleTransaction: s.singleTransaction || undefined,
    };
  },

  runCompare: async () => {
    const s = get();
    const opId = crypto.randomUUID();
    set({
      status: "comparing",
      error: undefined,
      plan: undefined,
      basePlan: undefined,
      replanning: false,
      warnings: [],
      comparedAt: undefined,
      comparedSignature: optionsSignature(s),
      excluded: new Set(),
      autoExcluded: new Map(),
      gatingNotice: undefined,
      selectedId: undefined,
      allowHazards: false,
      publish: { running: false, total: 0, applied: 0, log: [], done: false },
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
            basePlan: plan,
            warnings: e.data.warnings ?? [],
            comparedAt: Date.now(),
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
      const including = next.has(id) || s.autoExcluded.has(id);
      if (including) {
        next.delete(id);
        if (s.basePlan) for (const dep of dependenciesOf(s.basePlan, id)) next.delete(dep);
      } else {
        next.add(id);
      }
      const auto = computeAutoExcluded(s.basePlan, next);
      replanSoon(get);
      return { excluded: next, autoExcluded: auto, gatingNotice: gatingNotice(s, next, auto, including, new Set([id])) };
    }),

  // Group-level include/exclude (the tri-state checkbox on a group header).
  setIncluded: (ids, included) =>
    set((s) => {
      const next = new Set(s.excluded);
      if (included) {
        for (const id of ids) {
          next.delete(id);
          if (s.basePlan) for (const dep of dependenciesOf(s.basePlan, id)) next.delete(dep);
        }
      } else {
        for (const id of ids) next.add(id);
      }
      const auto = computeAutoExcluded(s.basePlan, next);
      replanSoon(get);
      return { excluded: next, autoExcluded: auto, gatingNotice: gatingNotice(s, next, auto, included, new Set(ids)) };
    }),

  clearGatingNotice: () => set({ gatingNotice: undefined }),

  // Ask the daemon what the migration is under the current selection: an
  // excluded object that must still be recreated for a dependency shows up as
  // such, and the statement count is the one a publish would run. The full
  // comparison stays the list's backbone so excluded objects remain visible.
  replan: async () => {
    const s = get();
    if (!s.basePlan || s.status === "comparing") return;
    const seq = ++replanSeq;
    set({ replanning: true });
    try {
      const res = await api.replan(s.request());
      if (seq !== replanSeq || get().basePlan !== s.basePlan) return; // superseded
      set({ plan: overlayPlan(s.basePlan, res.plan), replanning: false });
    } catch (e) {
      // No cached comparison on the daemon (restarted?) or a transport error:
      // keep showing the full comparison; publish still sends the selection.
      console.warn("replan failed:", e);
      if (seq === replanSeq) set({ replanning: false });
    }
  },

  select: (id) => set({ selectedId: id }),

  dismissWarning: (index) => set((s) => ({ warnings: s.warnings.filter((_, i) => i !== index) })),

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

// A selection change re-plans after a short pause so a run of clicks costs one
// round trip; the sequence number lets a late answer be ignored.
let replanTimer: ReturnType<typeof setTimeout> | undefined;
let replanSeq = 0;
function replanSoon(get: () => State) {
  if (replanTimer) clearTimeout(replanTimer);
  replanTimer = setTimeout(() => {
    replanTimer = undefined;
    void get().replan();
  }, 250);
}

// gatingNotice words what a toggle did beyond the rows the user clicked:
// dependents that were excluded with them, dependencies that came back with
// them. Undefined when nothing else moved.
function gatingNotice(
  s: State,
  nextExcluded: Set<string>,
  nextAuto: Map<string, string>,
  including: boolean,
  toggled: Set<string>,
): { text: string } | undefined {
  const name = (id: string) => {
    const c = s.basePlan?.changes.find((x) => x.id === id);
    return c ? `${c.objectType} ${c.schema ? c.schema + "." : ""}${c.name}` : id;
  };
  const list = (ids: string[]) => {
    const shown = ids.slice(0, 3).map(name).join(", ");
    return ids.length > 3 ? `${shown} and ${ids.length - 3} more` : shown;
  };
  const parts: string[] = [];
  if (including) {
    const deps = [...s.excluded].filter((id) => !nextExcluded.has(id) && !toggled.has(id));
    if (deps.length) parts.push(`Also included ${deps.length} dependenc${deps.length === 1 ? "y" : "ies"} it needs: ${list(deps)}`);
    const freed = [...s.autoExcluded.keys()].filter((id) => !nextAuto.has(id) && !toggled.has(id));
    if (freed.length) parts.push(`${freed.length} dependent change${freed.length === 1 ? " is" : "s are"} included again: ${list(freed)}`);
  } else {
    const gated = [...nextAuto.keys()].filter((id) => !s.autoExcluded.has(id) && !toggled.has(id));
    if (gated.length) parts.push(`Also excluded ${gated.length} change${gated.length === 1 ? "" : "s"} that depend${gated.length === 1 ? "s" : ""} on it: ${list(gated)}`);
  }
  return parts.length ? { text: parts.join(". ") + "." } : undefined;
}

// overlayPlan keeps the full comparison's object list (so excluded objects stay
// visible and re-includable) and takes everything that will actually run —
// statements, hazards, reasons, counts — from the re-planned migration. An
// object the migration no longer touches is marked omitted.
export function overlayPlan(base: PlanDoc, effective: PlanDoc): PlanDoc {
  const byId = new Map(effective.changes.map((c) => [c.id, c]));
  const changes: ObjectChange[] = base.changes.map((b) => {
    const e = byId.get(b.id);
    if (e) return e;
    return { ...b, statements: [], hazards: [], severity: "", reason: "", omitted: true };
  });
  const known = new Set(base.changes.map((b) => b.id));
  for (const e of effective.changes) if (!known.has(e.id)) changes.push(e);
  return { ...effective, changes };
}

function firstLine(sql: string): string {
  const i = sql.indexOf("\n");
  return i >= 0 ? sql.slice(0, i) + " …" : sql;
}

// Selectors / helpers used by components.
export const includedChanges = (plan: PlanDoc | undefined, excluded: Set<string>): ObjectChange[] =>
  (plan?.changes ?? []).filter((c) => !excluded.has(c.id));
