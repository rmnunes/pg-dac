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
}

export interface PlanResponse {
  plan: PlanDoc;
  warnings?: string[];
}

export interface ScriptResponse {
  sql: string;
  warnings?: string[];
}

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
    warnings?: string[];
  };
}

// Publish SSE payloads, re-emitted by the Rust supervisor as {event, data}.
export type PublishEventName = "start" | "progress" | "done" | "error" | "message";

export interface PublishEvent {
  event: PublishEventName;
  data: {
    statementCount?: number;
    dataLoss?: boolean;
    warnings?: string[];
    index?: number;
    total?: number;
    sql?: string;
    applied?: number;
    message?: string;
  };
}
