// Mirrors the pg-dac daemon's JSON 1:1 (engine.PlanDoc / ObjectChange / Hazard).

export type Severity = "error" | "warning" | "info" | "";

export interface Hazard {
  type: string;
  message: string;
  severity: Severity;
}

export interface Statement {
  sql: string;
  transactional: boolean;
  note?: string;
  hazards?: Hazard[];
}

export type ChangeKind = "add" | "drop" | "alter";

export interface ObjectChange {
  id: string;
  objectType: string;
  schema?: string;
  name: string;
  changeKind: ChangeKind;
  dependsOn?: string[];
  oldDefinition?: string;
  newDefinition?: string;
  statements: Statement[];
  hazards?: Hazard[];
  severity?: Severity;
  /** Why the engine touches the object beyond the plain diff (a dependent recreated, a rebuild). */
  reason?: string;
  /** UI only: the object is in the comparison but nothing runs for it under the current selection. */
  omitted?: boolean;
}

export interface HazardCount {
  code: string;
  severity: Severity;
  count: number;
}

export interface PlanDoc {
  formatVersion: number;
  statementCount: number;
  dataLoss: boolean;
  changes: ObjectChange[];
  hazardSummary?: HazardCount[];
  /** Refactorlog renames that run before the migration. */
  prelude?: Statement[];
}

/** The PlanDoc / daemon API format this UI understands (engine.PlanFormatVersion). */
export const EXPECTED_FORMAT_VERSION = 2;

/**
 * Which side of the comparison a warning is about: "target" is the database
 * that gets updated (the engine's --from), "source" the desired state (--to),
 * "plan" the generated migration itself.
 */
export type WarningSide = "target" | "source" | "plan" | "";

export interface Warning {
  side: WarningSide;
  message: string;
}

export interface PlanResponse {
  plan: PlanDoc;
  warnings?: Warning[];
}

export interface ScriptResponse {
  sql: string;
  warnings?: Warning[];
}

/** Compare options a project's pgdac.yaml `compare:` block can also set; "" = the project's default. */
export type PrivilegesMode = "" | "compare" | "ignore";
export type ColumnOrderMode = "" | "ignore" | "warn" | "rebuild";
export type CommentsMode = "" | "compare" | "ignore";
export type WhitespaceMode = "" | "ignore" | "compare";

export interface Health {
  status: string;
  version: string;
  formatVersion: number;
}

export type ProfileKind = "db" | "project" | "artifact";

export interface Profile {
  id: string;
  name: string;
  kind: ProfileKind;
  value: string;
}

export interface CompareRequest {
  from: string;
  to: string;
  opId?: string;
  schemas?: string[];
  targetMajor?: number;
  online?: boolean;
  allowHazards?: boolean;
  dropObjectsNotInSource?: boolean;
  includeIds?: string[];
  excludeIds?: string[];
  /** Fail unless a .pgdac source is signed by this key fingerprint. */
  requireKey?: string;
  /** Do not run the source's pre/post-deploy scripts on publish. */
  skipDeployScripts?: boolean;
  /** Publish even if the target changed outside pg-dac since its last baseline. */
  allowDrift?: boolean;
  /** Do not compare GRANT/REVOKE (privileges are still re-granted after a recreate). */
  ignorePrivileges?: boolean;
  /** What to do when a table's declared column order differs from the target's. */
  columnOrder?: "ignore" | "warn" | "rebuild";
  /** Ignore SQL comments inside routine bodies. */
  ignoreComments?: boolean;
  /** Compare routine bodies as exact text (whitespace counts). */
  compareWhitespace?: boolean;
  /** Object types left out of the comparison (SSDT ExcludeObjectTypes). */
  excludeObjectTypes?: string[];
  /** Values for $(name) placeholders in pre/post-deploy scripts. */
  variables?: Record<string, string>;
  /** Run consecutive transactional statements in one transaction (publish). */
  singleTransaction?: boolean;
}

// Compare SSE payloads, re-emitted by the Rust supervisor as {event, data}.
// "phase" narrates progress; "result" carries the PlanDoc; then "canceled" | "error".
export type CompareEventName = "phase" | "result" | "canceled" | "error" | "message";

export interface CompareEvent {
  event: CompareEventName;
  data: {
    step?: string;
    message?: string;
    source?: string;
    fromCount?: number;
    toCount?: number;
    plan?: PlanDoc;
    warnings?: Warning[];
  };
}

// Publish SSE payloads, re-emitted by the Rust supervisor as {event, data}.
export type PublishEventName = "start" | "progress" | "done" | "error" | "message";

export interface PublishEvent {
  event: PublishEventName;
  data: {
    statementCount?: number;
    dataLoss?: boolean;
    warnings?: Warning[];
    index?: number;
    total?: number;
    sql?: string;
    applied?: number;
    message?: string;
    deployScripts?: number;
    baselineRecorded?: boolean;
    /** Set on an error event when the publish was refused because of drift. */
    drift?: boolean;
  };
}
