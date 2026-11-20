// Thin typed wrappers over the Tauri commands exposed by src-tauri, plus the
// publish event subscription and native file/folder pickers.

import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { open } from "@tauri-apps/plugin-dialog";

import type {
  CompareEvent,
  CompareRequest,
  Health,
  PlanResponse,
  Profile,
  PublishEvent,
  ScriptResponse,
} from "./types";
import { mock } from "./mock";

// Outside the Tauri shell (plain `vite` in a browser, for UX work and audits)
// every command is served by the in-browser mock daemon instead.
const inTauri = typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;

export const health = (): Promise<Health> => (inTauri ? invoke("pgdac_health") : mock.health());

// Saved source profiles (persisted in the app config dir). Each call returns the
// full, updated list.
export const listProfiles = (): Promise<Profile[]> => (inTauri ? invoke("profiles_list") : mock.listProfiles());
export const addProfile = (profile: Profile): Promise<Profile[]> => (inTauri ? invoke("profile_add", { profile }) : mock.addProfile(profile));
export const removeProfile = (id: string): Promise<Profile[]> => (inTauri ? invoke("profile_remove", { id }) : mock.removeProfile(id));

// Compare streams its progress over the pgdac://compare event channel; this just
// kicks it off and returns immediately. The request's opId is used to cancel it.
export const startCompare = (req: CompareRequest): Promise<void> => (inTauri ? invoke("pgdac_plan", { req }) : mock.startCompare(req));

export const cancelCompare = (opId: string): Promise<unknown> => (inTauri ? invoke("pgdac_cancel", { opId }) : mock.cancelCompare());

export const onCompare = (cb: (e: CompareEvent) => void): Promise<UnlistenFn> =>
  inTauri ? listen<CompareEvent>("pgdac://compare", (event) => cb(event.payload)) : mock.onCompare(cb);

// Re-plans the last comparison for a changed selection without re-reading the
// sources (sub-second). Rejects when no comparison of these sources is cached.
export const replan = (req: CompareRequest): Promise<PlanResponse> => (inTauri ? invoke("pgdac_replan", { req }) : mock.replan(req));

export const script = (req: CompareRequest): Promise<ScriptResponse> => (inTauri ? invoke("pgdac_script", { req }) : mock.script(req));

export const startPublish = (req: CompareRequest): Promise<void> => (inTauri ? invoke("pgdac_publish", { req }) : mock.startPublish(req));

export const onPublish = (cb: (e: PublishEvent) => void): Promise<UnlistenFn> =>
  inTauri ? listen<PublishEvent>("pgdac://publish", (event) => cb(event.payload)) : mock.onPublish(cb);

export const pickFile = (filters?: { name: string; extensions: string[] }[]): Promise<string | null> =>
  inTauri ? open({ multiple: false, directory: false, filters }).then((r) => (typeof r === "string" ? r : null)) : mock.pickFile();

export const pickDirectory = (): Promise<string | null> =>
  inTauri ? open({ multiple: false, directory: true }).then((r) => (typeof r === "string" ? r : null)) : mock.pickDirectory();
