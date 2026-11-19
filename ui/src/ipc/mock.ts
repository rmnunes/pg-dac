// A browser-only stand-in for the Tauri commands, used when the frontend runs
// outside the desktop shell (plain `vite` in a browser): UX work, visual
// review and accessibility audits without a database or the daemon. It
// replays a real plan captured from the CLI (src/dev/fixture-plan.json) with
// the same event choreography the Rust supervisor relays, and keeps saved
// sources in localStorage. Never bundled into the desktop app's behavior: the
// client only routes here when Tauri's runtime is absent.

import type { CompareEvent, CompareRequest, Health, PlanDoc, PlanResponse, Profile, PublishEvent, ScriptResponse, Warning } from "./types";
import fixture from "../dev/fixture-plan.json";

type Listener<T> = (e: T) => void;
// Each subscription is its own entry (not the callback itself): React StrictMode
// mounts effects twice with the same stable callback, and the first mount's
// cleanup must not remove the second mount's subscription — Tauri's listen
// behaves the same way, returning a distinct unlisten per call.
const compareListeners = new Set<{ cb: Listener<CompareEvent> }>();
const publishListeners = new Set<{ cb: Listener<PublishEvent> }>();
const emitCompare = (e: CompareEvent) => compareListeners.forEach((l) => l.cb(e));
const emitPublish = (e: PublishEvent) => publishListeners.forEach((l) => l.cb(e));
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const PROFILES_KEY = "pgdac-mock-profiles";
const seedProfiles: Profile[] = [
  { id: "p-dev", name: "[DEV] Demo", kind: "db", value: "postgres://app_user@dev-pg.example.internal:5432/app?sslmode=require" },
  { id: "p-acc", name: "[ACC] Demo", kind: "db", value: "postgres://app_user@acc-pg.example.internal:5432/app?sslmode=require" },
  { id: "p-proj", name: "Demo project", kind: "project", value: "C:\\dev\\app\\schema" },
];
function loadProfiles(): Profile[] {
  try {
    const raw = localStorage.getItem(PROFILES_KEY);
    if (raw) return JSON.parse(raw) as Profile[];
  } catch {
    /* storage unavailable: fall through to the seed */
  }
  return seedProfiles;
}
function saveProfiles(p: Profile[]) {
  try {
    localStorage.setItem(PROFILES_KEY, JSON.stringify(p));
  } catch {
    /* ignore */
  }
}

const fixturePlan = fixture as unknown as PlanDoc;
const fixtureWarnings: Warning[] = [
  { side: "target", message: "13 default privileges (ALTER DEFAULT PRIVILEGES) scoped to these schemas present but not managed by pg-dac" },
  { side: "source", message: "7 default privileges (ALTER DEFAULT PRIVILEGES) scoped to these schemas present but not managed by pg-dac" },
  { side: "plan", message: 'table "qa"."reviews_audit": column finish_date declared in a position the target does not have (the target appended it later); PostgreSQL cannot reorder columns in place, so the order is left as is. To make the target match, set compare.columnOrder: rebuild (the table is rebuilt); to make the project match the target instead, update the project from it: pg-dac publish --from <project> --to <target>' },
];

let cancelled = false;

export const mock = {
  health: async (): Promise<Health> => ({ status: "ok", version: "0.9.0-dev (browser mock)", formatVersion: fixturePlan.formatVersion }),

  listProfiles: async (): Promise<Profile[]> => loadProfiles(),
  addProfile: async (profile: Profile): Promise<Profile[]> => {
    const p = [...loadProfiles(), { ...profile, id: profile.id || `p-${Date.now()}` }];
    saveProfiles(p);
    return p;
  },
  removeProfile: async (id: string): Promise<Profile[]> => {
    const p = loadProfiles().filter((x) => x.id !== id);
    saveProfiles(p);
    return p;
  },

  startCompare: async (req: CompareRequest): Promise<void> => {
    cancelled = false;
    void (async () => {
      emitCompare({ event: "phase", data: { step: "read-from", message: "Reading Target…", source: req.from } });
      await sleep(600);
      if (cancelled) return emitCompare({ event: "canceled", data: {} });
      emitCompare({ event: "phase", data: { step: "read-to", message: "Reading Source…", source: req.to, fromCount: 915 } });
      await sleep(600);
      if (cancelled) return emitCompare({ event: "canceled", data: {} });
      emitCompare({ event: "phase", data: { step: "diff", message: "Diffing and generating plan…", toCount: 914 } });
      await sleep(400);
      if (cancelled) return emitCompare({ event: "canceled", data: {} });
      const excluded = new Set(req.excludeIds ?? []);
      const plan: PlanDoc = { ...fixturePlan, changes: fixturePlan.changes.filter((c) => !excluded.has(c.id)) };
      plan.statementCount = plan.changes.reduce((n, c) => n + c.statements.length, 0);
      emitCompare({ event: "result", data: { plan, warnings: fixtureWarnings } });
    })();
  },
  cancelCompare: async (): Promise<unknown> => {
    cancelled = true;
    return { canceled: true };
  },
  onCompare: async (cb: Listener<CompareEvent>) => {
    const entry = { cb };
    compareListeners.add(entry);
    return () => compareListeners.delete(entry);
  },

  replan: async (req: CompareRequest): Promise<PlanResponse> => {
    await sleep(150);
    const excluded = new Set(req.excludeIds ?? []);
    const plan: PlanDoc = { ...fixturePlan, changes: fixturePlan.changes.filter((c) => !excluded.has(c.id)) };
    plan.statementCount = plan.changes.reduce((n, c) => n + c.statements.length, 0);
    plan.dataLoss = plan.changes.some((c) => c.hazards?.some((h) => h.type === "DELETES_DATA"));
    return { plan, warnings: fixtureWarnings };
  },

  script: async (req: CompareRequest): Promise<ScriptResponse> => {
    const excluded = new Set(req.excludeIds ?? []);
    const body = fixturePlan.changes
      .filter((c) => !excluded.has(c.id))
      .flatMap((c) => c.statements.map((s) => (s.note ? `-- ${s.note}\n` : "") + s.sql))
      .join("\n\n");
    return { sql: `-- pg-dac migration script (browser mock)\nSET check_function_bodies = false;\n\n${body}\n`, warnings: fixtureWarnings };
  },

  startPublish: async (req: CompareRequest): Promise<void> => {
    void (async () => {
      const excluded = new Set(req.excludeIds ?? []);
      const stmts = fixturePlan.changes.filter((c) => !excluded.has(c.id)).flatMap((c) => c.statements);
      emitPublish({ event: "start", data: { statementCount: stmts.length, dataLoss: fixturePlan.dataLoss, deployScripts: 1, warnings: fixtureWarnings } });
      await sleep(300);
      emitPublish({ event: "progress", data: { index: 0, total: stmts.length, sql: "-- pre-deploy script: pre/001_roles.sql" } });
      for (let i = 0; i < stmts.length; i++) {
        await sleep(120);
        emitPublish({ event: "progress", data: { index: i + 1, total: stmts.length, sql: stmts[i].sql } });
      }
      emitPublish({ event: "done", data: { applied: stmts.length, deployScripts: 1, baselineRecorded: true } });
    })();
  },
  onPublish: async (cb: Listener<PublishEvent>) => {
    const entry = { cb };
    publishListeners.add(entry);
    return () => publishListeners.delete(entry);
  },

  buildArtifact: async (): Promise<string> => "Wrote app.pgdac (browser mock)",
  inspectArtifact: async (): Promise<string> => "Name: app\nIntegrity: OK (browser mock)",
  pickFile: async (): Promise<string | null> => window.prompt("Path of a .pgdac artifact (browser mock)", "C:\\artifacts\\app.pgdac"),
  pickDirectory: async (): Promise<string | null> => window.prompt("Project directory (browser mock)", "C:\\dev\\app\\schema"),
};
