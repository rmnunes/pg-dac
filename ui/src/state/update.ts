// In-app updates: the check the app makes on its own (shortly after launch,
// then every few hours, silently), the manual check from the status bar, and
// the user-driven download and restart. The work happens in the Tauri layer
// (src-tauri/src/update.rs), which stops the engine before the installer runs.

import { create } from "zustand";

import { checkForUpdate, downloadUpdate, installUpdate, onUpdateProgress } from "../ipc/client";
import type { UpdateInfo } from "../ipc/types";

export type UpdatePhase =
  | { kind: "idle" }
  | { kind: "checking"; manual: boolean }
  /** A newer version exists. `error` is set when a download attempt failed. */
  | { kind: "available"; info: UpdateInfo; error?: string }
  | { kind: "downloading"; info: UpdateInfo; received: number; total: number | null }
  /** Downloaded and verified. `error` is set when an install attempt failed. */
  | { kind: "ready"; info: UpdateInfo; error?: string }
  | { kind: "installing"; info: UpdateInfo }
  /** Only after a manual check. */
  | { kind: "uptodate" }
  /** Only after a manual check: automatic checks fail silently. */
  | { kind: "failed"; message: string };

interface UpdateStore {
  phase: UpdatePhase;
  /** The version the user postponed with "Later"; automatic checks do not offer it again. */
  snoozed?: string;
  check: (manual: boolean) => Promise<void>;
  download: () => Promise<void>;
  install: () => Promise<void>;
  dismiss: () => void;
}

/** First automatic check after launch, then the interval between checks. */
export const FIRST_CHECK_MS = 3_000;
export const CHECK_EVERY_MS = 6 * 60 * 60 * 1000;

const message = (e: unknown): string => (e instanceof Error ? e.message : String(e));

export const useUpdates = create<UpdateStore>((set, get) => ({
  phase: { kind: "idle" },

  check: async (manual) => {
    const { phase, snoozed } = get();
    // Never replace an update the user is acting on, and never run two checks.
    if (phase.kind === "checking" || phase.kind === "downloading" || phase.kind === "installing" || phase.kind === "ready") return;
    if (!manual && phase.kind === "available") return;
    set({ phase: { kind: "checking", manual } });
    try {
      const info = await checkForUpdate();
      if (info && (manual || info.version !== snoozed)) set({ phase: { kind: "available", info } });
      else set({ phase: manual ? { kind: "uptodate" } : { kind: "idle" } });
    } catch (e) {
      // An automatic check that fails (offline, endpoint unreachable, a build
      // without the signing key) is not worth interrupting anyone for.
      set({ phase: manual ? { kind: "failed", message: message(e) } : { kind: "idle" } });
    }
  },

  download: async () => {
    const phase = get().phase;
    if (phase.kind !== "available") return;
    const info = phase.info;
    set({ phase: { kind: "downloading", info, received: 0, total: null } });
    const unlisten = await onUpdateProgress((p) => {
      const cur = get().phase;
      if (cur.kind === "downloading") set({ phase: { ...cur, received: p.received, total: p.total ?? null } });
    });
    try {
      await downloadUpdate();
      set({ phase: { kind: "ready", info } });
    } catch (e) {
      set({ phase: { kind: "available", info, error: message(e) } });
    } finally {
      unlisten();
    }
  },

  install: async () => {
    const phase = get().phase;
    if (phase.kind !== "ready") return;
    const info = phase.info;
    set({ phase: { kind: "installing", info } });
    try {
      // On success the app restarts into the new version and this never returns.
      await installUpdate();
    } catch (e) {
      set({ phase: { kind: "ready", info, error: message(e) } });
    }
  },

  dismiss: () => {
    const phase = get().phase;
    if (phase.kind === "downloading" || phase.kind === "installing") return;
    set({ phase: { kind: "idle" }, snoozed: "info" in phase ? phase.info.version : get().snoozed });
  },
}));

/** Starts the automatic checks; returns the function that stops them. */
export function startUpdateChecks(): () => void {
  const check = () => void useUpdates.getState().check(false);
  const first = setTimeout(check, FIRST_CHECK_MS);
  const every = setInterval(check, CHECK_EVERY_MS);
  return () => {
    clearTimeout(first);
    clearInterval(every);
  };
}
