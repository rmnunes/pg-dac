// Thin typed wrappers over the Tauri commands exposed by src-tauri, plus the
// publish event subscription and native file/folder pickers.

import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { open } from "@tauri-apps/plugin-dialog";

import type {
  CompareEvent,
  CompareRequest,
  Health,
  Profile,
  PublishEvent,
  ScriptResponse,
} from "./types";

export const health = (): Promise<Health> => invoke("pgdac_health");

// Saved source profiles (persisted in the app config dir). Each call returns the
// full, updated list.
export const listProfiles = (): Promise<Profile[]> => invoke("profiles_list");
export const addProfile = (profile: Profile): Promise<Profile[]> => invoke("profile_add", { profile });
export const removeProfile = (id: string): Promise<Profile[]> => invoke("profile_remove", { id });

// Compare streams its progress over the pgdac://compare event channel; this just
// kicks it off and returns immediately. The request's opId is used to cancel it.
export const startCompare = (req: CompareRequest): Promise<void> => invoke("pgdac_plan", { req });

export const cancelCompare = (opId: string): Promise<unknown> => invoke("pgdac_cancel", { opId });

export const onCompare = (cb: (e: CompareEvent) => void): Promise<UnlistenFn> =>
  listen<CompareEvent>("pgdac://compare", (event) => cb(event.payload));

export const script = (req: CompareRequest): Promise<ScriptResponse> => invoke("pgdac_script", { req });

export const startPublish = (req: CompareRequest): Promise<void> => invoke("pgdac_publish", { req });

export const buildArtifact = (args: {
  from: string;
  out: string;
  name?: string;
  version?: string;
  noSign?: boolean;
}): Promise<string> => invoke("pgdac_build", args);

export const inspectArtifact = (path: string): Promise<string> => invoke("pgdac_inspect", { path });

export const onPublish = (cb: (e: PublishEvent) => void): Promise<UnlistenFn> =>
  listen<PublishEvent>("pgdac://publish", (event) => cb(event.payload));

export const pickFile = (filters?: { name: string; extensions: string[] }[]): Promise<string | null> =>
  open({ multiple: false, directory: false, filters }).then((r) => (typeof r === "string" ? r : null));

export const pickDirectory = (): Promise<string | null> =>
  open({ multiple: false, directory: true }).then((r) => (typeof r === "string" ? r : null));
