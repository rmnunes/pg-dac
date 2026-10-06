import { AlertTriangle, X } from "lucide-react";

import { useStore } from "../state/store";
import { sideLabel } from "../vocab";

// The engine's warnings ("notes": they do not stop anything) in a collapsible
// panel above the status bar, each dismissable. Hidden when there are none.
export function NotesPanel({ open }: { open: boolean }) {
  const { warnings, dismissWarning } = useStore();
  if (!open || warnings.length === 0) return null;
  return (
    <section className="notes" id="notes-panel" aria-label="Notes">
      {warnings.map((w, i) => (
        <div className="note" key={i}>
          <AlertTriangle className="icon" size={14} aria-hidden="true" />
          {w.side && <span className="side">{sideLabel(w.side)}</span>}
          <span className="msg">{w.message}</span>
          <button type="button" className="icon-btn" aria-label="Dismiss this note" title="Dismiss" onClick={() => dismissWarning(i)}>
            <X size={14} aria-hidden="true" />
          </button>
        </div>
      ))}
    </section>
  );
}
