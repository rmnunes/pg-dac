import { AlertTriangle, Info, OctagonAlert } from "lucide-react";

import { optionsStale, useStore } from "../state/store";
import { plural } from "../vocab";

export interface EngineInfo {
  version?: string;
  note?: string;
}

const fmtTime = (t: number): string => new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

export function StatusBar({ notesOpen, onToggleNotes, engine }: { notesOpen: boolean; onToggleNotes: () => void; engine: EngineInfo }) {
  const plan = useStore((s) => s.plan);
  const comparedAt = useStore((s) => s.comparedAt);
  const replanning = useStore((s) => s.replanning);
  const warningCount = useStore((s) => s.warnings.length);
  const stale = useStore(optionsStale);

  const counts = (plan?.changes ?? []).reduce<Record<string, number>>((a, c) => {
    a[c.changeKind] = (a[c.changeKind] ?? 0) + 1;
    return a;
  }, {});
  const parts: string[] = [];
  if (plan) {
    parts.push(plural(plan.statementCount, "statement"));
    const kinds = [counts.drop ? `${counts.drop} drop` : "", counts.alter ? `${counts.alter} change` : "", counts.add ? `${counts.add} add` : ""].filter(Boolean);
    if (kinds.length) parts.push(kinds.join(" · "));
    if (plan.prelude?.length) parts.push(plural(plan.prelude.length, "refactorlog rename"));
    if (comparedAt) parts.push(`compared at ${fmtTime(comparedAt)}`);
  }

  return (
    <footer className="statusbar" aria-label="Status">
      <span role="status">
        {plan ? parts.join(" · ") : "No comparison yet"}
        {replanning && <span className="muted"> · updating the plan…</span>}
      </span>
      {plan?.dataLoss && (
        <span className="flag">
          <OctagonAlert size={13} aria-hidden="true" /> Deletes data
        </span>
      )}
      {stale && (
        <span className="stale">
          <AlertTriangle size={13} aria-hidden="true" /> Options or sources changed — compare again
        </span>
      )}
      <div className="right">
        {warningCount > 0 && (
          <button type="button" aria-expanded={notesOpen} aria-controls="notes-panel" onClick={onToggleNotes} title={notesOpen ? "Hide the notes" : "Show the notes"}>
            <AlertTriangle size={13} aria-hidden="true" />
            Notes ({warningCount})
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
