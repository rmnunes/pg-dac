// A browser-only stand-in for the Tauri commands, used when the frontend runs
// outside the desktop shell (plain `vite` in a browser): UX work, visual
// review and accessibility audits without a database or the daemon. It
// replays a real plan captured from the CLI (src/dev/fixture-plan.json) with
// the same event choreography the Rust supervisor relays and the same refusals
// the daemon makes (a re-plan of sources that were not compared, a publish
// with data loss that was not approved), and keeps saved sources in
// localStorage. An update is offered when the page URL carries ?update=<version>
// (or a test calls mock.offerUpdate). Never bundled into the desktop app's behavior: the client only
// routes here when Tauri's runtime is absent.

import type { CompareEvent, CompareRequest, Health, HazardCount, PlanDoc, PlanResponse, Profile, PublishEvent, ScriptResponse, Severity, UpdateInfo, UpdateProgress, Warning } from "./types";
import fixture from "../dev/fixture-plan.json";
import pinnedVersion from "../../PGDAC_VERSION?raw";

type Listener<T> = (e: T) => void;
// Each subscription is its own entry (not the callback itself): React StrictMode
// mounts effects twice with the same stable callback, and the first mount's
// cleanup must not remove the second mount's subscription — Tauri's listen
// behaves the same way, returning a distinct unlisten per call.
const compareListeners = new Set<{ cb: Listener<CompareEvent> }>();
const publishListeners = new Set<{ cb: Listener<PublishEvent> }>();
const emitCompare = (e: CompareEvent) => compareListeners.forEach((l) => l.cb(e));
const emitPublish = (e: PublishEvent) => publishListeners.forEach((l) => l.cb(e));
const updateListeners = new Set<{ cb: Listener<UpdateProgress> }>();
const emitUpdate = (p: UpdateProgress) => updateListeners.forEach((l) => l.cb(p));

// The mock engine is a dev build of the pinned release, like a local sidecar.
const MOCK_VERSION = pinnedVersion.trim();
let offeredUpdate: string | null = typeof location !== "undefined" ? new URLSearchParams(location.search).get("update") : null;
let downloaded = false;
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

/**
 * The migration under a selection, as the engine computes it: excluded changes
 * drop out, except one the engine must still recreate because a change it
 * depends on runs (the fixture marks those with a reason); counts, the data
 * loss flag and the hazard summary follow what remains.
 */
export function effectivePlan(excludeIds: string[] = []): PlanDoc {
  const excluded = new Set(excludeIds);
  const running = new Set(fixturePlan.changes.filter((c) => !excluded.has(c.id)).map((c) => c.id));
  const changes = fixturePlan.changes.filter((c) => running.has(c.id) || (!!c.reason && (c.dependsOn ?? []).some((d) => running.has(d))));
  const summary = new Map<string, HazardCount>();
  for (const c of changes) {
    for (const h of c.hazards ?? []) {
      const s = summary.get(h.type) ?? { code: h.type, severity: h.severity as Severity, count: 0 };
      s.count++;
      summary.set(h.type, s);
    }
  }
  return {
    ...fixturePlan,
    changes,
    statementCount: changes.reduce((n, c) => n + c.statements.length, 0),
    dataLoss: changes.some((c) => c.hazards?.some((h) => h.type === "DELETES_DATA")),
    hazardSummary: [...summary.values()],
  };
}

// What the daemon remembers of the last compare, so a re-plan of other sources
// is refused the same way (the Rust supervisor reports it as "daemon returned
// 409: …").
let compared: { from: string; to: string } | undefined;
let cancelled = false;

export const mock = {
  health: async (): Promise<Health> => ({ status: "ok", version: `${MOCK_VERSION}-dev+mock`, formatVersion: fixturePlan.formatVersion }),

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
      compared = { from: req.from, to: req.to };
      emitCompare({ event: "result", data: { plan: effectivePlan(req.excludeIds), warnings: fixtureWarnings } });
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
    if (!compared || compared.from !== req.from || compared.to !== req.to) {
      // Tauri's invoke rejects with the command's error string, not an Error.
      return Promise.reject("daemon returned 409: no comparison of these sources to re-plan; run Compare first");
    }
    return { plan: effectivePlan(req.excludeIds), warnings: fixtureWarnings };
  },

  script: async (req: CompareRequest): Promise<ScriptResponse> => {
    const body = effectivePlan(req.excludeIds)
      .changes.flatMap((c) => c.statements.map((s) => (s.note ? `-- ${s.note}\n` : "") + s.sql))
      .join("\n\n");
    return { sql: `-- pg-dac migration script (browser mock)\nSET check_function_bodies = false;\n\n${body}\n`, warnings: fixtureWarnings };
  },

  startPublish: async (req: CompareRequest): Promise<void> => {
    void (async () => {
      const plan = effectivePlan(req.excludeIds);
      const stmts = plan.changes.flatMap((c) => c.statements);
      emitPublish({ event: "start", data: { statementCount: stmts.length, dataLoss: plan.dataLoss, deployScripts: 1, warnings: fixtureWarnings } });
      await sleep(300);
      if (plan.dataLoss && !req.allowHazards) {
        emitPublish({ event: "error", data: { applied: 0, total: stmts.length, message: "refusing to publish: the migration contains data-loss or unsupported changes; set allowHazards to proceed" } });
        return;
      }
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

  /** Test hook: the version the mock offers (null for none). */
  offerUpdate: (version: string | null) => {
    offeredUpdate = version;
    downloaded = false;
  },
  checkForUpdate: async (): Promise<UpdateInfo | null> => {
    await sleep(300);
    downloaded = false;
    return offeredUpdate ? { version: offeredUpdate, currentVersion: MOCK_VERSION, notes: null } : null;
  },
  downloadUpdate: async (): Promise<void> => {
    if (!offeredUpdate) return Promise.reject("there is no update to download; check for updates first");
    const total = 18 * 1024 * 1024;
    for (let received = 0; received < total; ) {
      await sleep(150);
      received = Math.min(total, received + 2 * 1024 * 1024);
      emitUpdate({ received, total });
    }
    downloaded = true;
  },
  installUpdate: async (): Promise<void> => {
    if (!downloaded) return Promise.reject("the update has not been downloaded yet");
    await sleep(800);
    // The desktop app restarts into the new version; the mock reloads without the offer.
    const url = new URL(location.href);
    url.searchParams.delete("update");
    location.replace(url.toString());
  },
  onUpdateProgress: async (cb: Listener<UpdateProgress>) => {
    const entry = { cb };
    updateListeners.add(entry);
    return () => void updateListeners.delete(entry);
  },

  pickFile: async (): Promise<string | null> => window.prompt("Path of a .pgdac artifact (browser mock)", "C:\\artifacts\\app.pgdac"),
  pickDirectory: async (): Promise<string | null> => window.prompt("Project directory (browser mock)", "C:\\dev\\app\\schema"),
};
