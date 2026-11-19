// The GUI's vocabulary: human copy for the engine's hazard codes, warning
// sides and change kinds. The engine speaks in codes (safety.HazardType) and a
// per-statement explanation; the GUI adds a title, a short badge label and a
// standing description so a reader does not have to decode
// ACQUIRES_ACCESS_EXCLUSIVE_LOCK.

import type { ChangeKind, Hazard, ObjectChange, Severity, WarningSide } from "./ipc/types";

export interface HazardInfo {
  title: string;
  /** Badge text in the change list (lower case, two words at most). */
  short: string;
  description: string;
}

export const HAZARDS: Record<string, HazardInfo> = {
  DELETES_DATA: {
    title: "Deletes data",
    short: "data loss",
    description: "The statement destroys rows or a column's values. Publishing requires an explicit acknowledgement.",
  },
  UNSUPPORTED_CHANGE: {
    title: "Cannot be applied in place",
    short: "manual change",
    description:
      "PostgreSQL cannot make this change in place (dropping or reordering enum values, altering a type that is in use, re-partitioning a table). The Target will not converge until the change is made by hand.",
  },
  MISSING_ROLE: {
    title: "Role missing on the Target",
    short: "missing role",
    description:
      "A GRANT names a role the Target cluster does not have. The statement would fail, so publishing is blocked until the role exists or the grant leaves the Source.",
  },
  ACQUIRES_ACCESS_EXCLUSIVE_LOCK: {
    title: "Takes an ACCESS EXCLUSIVE lock",
    short: "exclusive lock",
    description:
      "Queues ahead of every later query on the table while it waits for in-flight transactions: fast on its own, dangerous behind a long-running reader. Online mode adds a lock timeout and retries.",
  },
  ACQUIRES_SHARE_ROW_EXCLUSIVE_LOCK: {
    title: "Takes a SHARE ROW EXCLUSIVE lock",
    short: "write lock",
    description: "Blocks writes to the table, and for a foreign key to the referenced table too, while the constraint is validated.",
  },
  ACQUIRES_SHARE_LOCK: {
    title: "Takes a SHARE lock",
    short: "share lock",
    description: "Blocks writes but not reads for the duration of the statement.",
  },
  INDEX_BUILD: {
    title: "Builds an index",
    short: "index build",
    description:
      "CPU and I/O heavy, and long-running on a large table. Online mode builds it CONCURRENTLY, outside a transaction; a changed index is rebuilt under a temporary name while the current one keeps serving queries, then swapped in.",
  },
  INDEX_DROPPED: {
    title: "Drops an index",
    short: "index dropped",
    description:
      "Queries that relied on the index may slow down. An index whose definition changed is not dropped first: online it is rebuilt under a temporary name and swapped in; offline it is dropped and recreated inside one transaction.",
  },
  TABLE_REWRITE: {
    title: "Rewrites the table",
    short: "rewrite",
    description: "The whole table and its indexes are rewritten under an ACCESS EXCLUSIVE lock; time and disk scale with the table size.",
  },
  IMPACTS_DATABASE_PERFORMANCE: {
    title: "Temporary performance impact",
    short: "performance",
    description: "The operation loads the server while it runs.",
  },
  CORRECTNESS: {
    title: "Check the intent",
    short: "check intent",
    description: "The generated statement may not mean what the Source intends. Review it before publishing.",
  },
  UPGRADING_EXTENSION_VERSION: {
    title: "Upgrades an extension",
    short: "extension upgrade",
    description: "ALTER EXTENSION … UPDATE runs the extension's own upgrade scripts.",
  },
};

function humanize(code: string): string {
  const s = code.replace(/_/g, " ").toLowerCase();
  return s.charAt(0).toUpperCase() + s.slice(1);
}

export function hazardInfo(code: string): HazardInfo {
  return HAZARDS[code] ?? { title: humanize(code), short: humanize(code).toLowerCase(), description: "" };
}

const RANK: Record<string, number> = { error: 3, warning: 2, info: 1, "": 0 };
export const severityRank = (s?: Severity): number => RANK[s ?? ""] ?? 0;

/** A change's hazards, most severe first. */
export function sortedHazards(c: ObjectChange): Hazard[] {
  return [...(c.hazards ?? [])].sort((a, b) => severityRank(b.severity) - severityRank(a.severity));
}

/** The most severe hazard of a change (the row badge). */
export const topHazard = (c: ObjectChange): Hazard | undefined => sortedHazards(c)[0];

// Warnings arrive tagged with the side they concern; the GUI's names for the
// engine's --from / --to are Target / Source.
export function sideLabel(side: WarningSide | string): string {
  switch (side) {
    case "target":
      return "Target";
    case "source":
      return "Source";
    case "plan":
      return "Plan";
    default:
      return side;
  }
}

export const KIND_ORDER: ChangeKind[] = ["drop", "alter", "add"];

export function kindLabel(k: ChangeKind): string {
  return k === "add" ? "Add" : k === "drop" ? "Drop" : "Change";
}

export const kindGlyph = (k: ChangeKind): string => (k === "add" ? "+" : k === "drop" ? "−" : "~");

export const objectName = (c: { schema?: string; name: string }): string => (c.schema ? c.schema + "." : "") + c.name;

/**
 * A change whose definition text is identical on both sides: the object is
 * touched for another reason (recreated because something it depends on
 * changes, or an attribute the definition does not show).
 */
export const unchangedText = (c: ObjectChange): boolean => c.changeKind === "alter" && !!c.oldDefinition && c.oldDefinition === c.newDefinition;
