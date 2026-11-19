import { AlertTriangle, Info, OctagonAlert } from "lucide-react";

import { optionsStale, useStore } from "../state/store";

export interface EngineInfo {
  version?: string;
  note?: string;
}

const fmtTime = (t: number): string => new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

export function StatusBar({ notesOpen, onToggleNotes, engine }: { notesOpen: boolean; onToggleNotes: () => void; engine: EngineInfo }) {
  const s = useStore();
  const plan = s.plan;
  const stale = optionsStale(s);
  const counts = (plan?.changes ?? []).reduce<Record<string, number>>((a, c) => {
    a[c.changeKind] = (a[c.changeKind] ?? 0) + 1;
    return a;
  }, {});
  const parts: string[] = [];
  if (plan) {
    parts.push(`${plan.statementCount} statement${plan.statementCount === 1 ? "" : "s"}`);
    const kinds = [counts.drop ? `${counts.drop} drop` : "", counts.alter ? `${counts.alter} change` : "", counts.add ? `${counts.add} add` : ""].filter(Boolean);
    if (kinds.length) parts.push(kinds.join(" · "));
    if (plan.prelude?.length) parts.push(`${plan.prelude.length} refactorlog rename${plan.prelude.length === 1 ? "" : "s"}`);
    if (s.comparedAt) parts.push(`compared at ${fmtTime(s.comparedAt)}`);
  }

  return (
    <footer className="statusbar" aria-label="Status">
      <span role="status">{plan ? parts.join(" · ") : "No comparison yet"}</span>
      {s.replanning && <span className="muted">updating the plan…</span>}
      {plan?.dataLoss && (
        <span className="flag">
          <OctagonAlert size={13} aria-hidden="true" /> Deletes data
        </span>
      )}
      {stale && (
        <span className="kind-alter">
          <AlertTriangle size={13} aria-hidden="true" style={{ verticalAlign: -2 }} /> Options or sources changed — compare again
        </span>
      )}
      <div className="right">
        {s.warnings.length > 0 && (
          <button type="button" aria-expanded={notesOpen} aria-controls="notes-panel" onClick={onToggleNotes} title={notesOpen ? "Hide the notes" : "Show the notes"}>
            <AlertTriangle size={13} aria-hidden="true" />
            Notes ({s.warnings.length})
          </button>
        )}
        {(engine.version || engine.note) && (
          <span className={"engine" + (engine.note ? " warn" : "")} title={engine.note}>
            {engine.note && <Info size={13} aria-hidden="true" />}
            engine {engine.version ?? "unavailable"}
            {engine.note && <span className="sr-only">: {engine.note}</span>}
          </span>
        )}
      </div>
    </footer>
  );
}
