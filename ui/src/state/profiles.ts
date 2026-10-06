// Helpers for saved source profiles. A "spec" (the value held in store.from /
// store.to) is either a raw typed value (a path or a connection string) or a
// `profile:<id>` reference. These helpers resolve a spec to the real value to
// send to the daemon, and — crucially — derive a *masked* label for display so a
// connection string's password is never rendered.

import type { Profile, ProfileKind } from "../ipc/types";

const PREFIX = "profile:";

export const isProfileRef = (spec: string): boolean => spec.startsWith(PREFIX);
export const profileRef = (id: string): string => PREFIX + id;

export const looksLikeDsn = (s: string): boolean =>
  /^postgres(ql)?:\/\//i.test(s.trim()) || /(^|;)\s*(host|server|data source)\s*=/i.test(s);

// dbDisplay extracts a "database @ host" label from a connection string, dropping
// the user, password, port, and any other parameters. Handles the URL form
// (postgres://user:pass@host:port/db) and the key/value form (Host=…;Database=…).
export function dbDisplay(dsn: string): string {
  const s = dsn.trim();
  const scheme = /^postgres(?:ql)?:\/\//i.exec(s);
  if (scheme) {
    const rest = s.slice(scheme[0].length);
    // Split the authority (host part) from the path/query, then drop the whole
    // userinfo (everything up to the LAST '@' in the authority). Stripping the
    // entire userinfo — not just a non-'@' prefix — means a password containing
    // '@' or ':' can never leak into the displayed host.
    const sep = rest.search(/[/?#]/);
    const authority = sep < 0 ? rest : rest.slice(0, sep);
    const tail = sep < 0 ? "" : rest.slice(sep);
    const at = authority.lastIndexOf("@");
    const hostPort = at >= 0 ? authority.slice(at + 1) : authority;
    const host = hostPort.replace(/:\d+$/, ""); // drop :port
    const db = tail.startsWith("/") ? tail.slice(1).split(/[?#]/)[0] : "";
    return db ? `${db} @ ${host}` : host || "database";
  }
  const kv = (k: string) => {
    const m = new RegExp(`(?:^|;)\\s*${k}\\s*=\\s*([^;]+)`, "i").exec(s);
    return m ? m[1].trim() : "";
  };
  const host = kv("host") || kv("server") || kv("data source");
  const db = kv("database") || kv("initial catalog");
  if (host || db) return db ? `${db} @ ${host}` : host;
  return "database";
}

export const baseName = (p: string): string => p.split(/[\\/]/).filter(Boolean).pop() || p;

// maskValue is the safe, secret-free label for a profile's value.
export function maskValue(kind: ProfileKind, value: string): string {
  if (kind === "db") return dbDisplay(value);
  if (kind === "artifact") return baseName(value);
  return value; // a project directory path is not a secret
}

export function inferKind(value: string): ProfileKind {
  if (looksLikeDsn(value)) return "db";
  if (/\.pgdac$/i.test(value.trim())) return "artifact";
  return "project";
}

export const defaultName = (value: string): string => maskValue(inferKind(value), value);

const find = (spec: string, profiles: Profile[]): Profile | undefined =>
  isProfileRef(spec) ? profiles.find((p) => p.id === spec.slice(PREFIX.length)) : undefined;

// resolveSpec → the actual value to send to the daemon (the real DSN / path).
export function resolveSpec(spec: string, profiles: Profile[]): string {
  const p = find(spec, profiles);
  return p ? p.value : spec;
}

// displaySpec → a safe label to render. Never returns a secret: profile refs use
// the saved name + masked value; a raw typed DSN is masked to "database @ host".
export function displaySpec(spec: string, profiles: Profile[]): string {
  if (!spec) return "—";
  const p = find(spec, profiles);
  if (p) return `${p.name} — ${maskValue(p.kind, p.value)}`;
  return looksLikeDsn(spec) ? dbDisplay(spec) : spec;
}

// specIsDb → whether a (possibly profile) spec refers to a live database.
export function specIsDb(spec: string, profiles: Profile[]): boolean {
  const p = find(spec, profiles);
  return p ? p.kind === "db" : looksLikeDsn(spec);
}
