import { AlertTriangle, X } from "lucide-react";

import { useStore } from "../state/store";
import type { Warning } from "../ipc/types";
import { sideLabel } from "../vocab";

// Stable keys for warnings: the same side and message twice get a counter.
function keyed(warnings: Warning[]): [string, Warning][] {
  const seen = new Map<string, number>();
  return warnings.map((w) => {
    const base = `${w.side}:${w.message}`;
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    return [n ? `${base}#${n}` : base, w];
  });
}

// The engine's warnings ("notes": they do not stop anything) in a collapsible
// panel above the status bar, each dismissable. Hidden when there are none.
export function NotesPanel({ open }: { open: boolean }) {
  const warnings = useStore((s) => s.warnings);
  const dismissWarning = useStore((s) => s.dismissWarning);
  if (!open || warnings.length === 0) return null;
  return (
    <section className="notes" id="notes-panel" aria-label="Notes">
      {keyed(warnings).map(([key, w]) => (
        <div className="note" key={key}>
          <AlertTriangle className="icon" size={14} aria-hidden="true" />
          {w.side && <span className="side">{sideLabel(w.side)}</span>}
          <span className="msg">{w.message}</span>
          <button type="button" className="icon-btn" aria-label="Dismiss this note" title="Dismiss" onClick={() => dismissWarning(w)}>
            <X size={14} aria-hidden="true" />
          </button>
        </div>
      ))}
    </section>
  );
}
