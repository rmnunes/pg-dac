import { Check } from "lucide-react";

import { useStore } from "../state/store";
import { displaySpec } from "../state/profiles";
import { Dialog } from "./Dialog";

// Shown while a compare runs: it narrates what the daemon is doing (reading
// each side, then diffing/planning) and offers Cancel, which aborts the work on
// the daemon. It closes itself when the result arrives; a canceled compare
// leaves a dismissible "Canceled." state. Escape cancels while running.
export function CompareProgress() {
  const { compareProgress: cp, cancelCompare, set, profiles } = useStore();
  if (!cp.running && !cp.canceled) return null;

  const close = () => {
    if (cp.running) cancelCompare();
    else set({ compareProgress: { ...cp, canceled: false } });
  };

  return (
    <Dialog title={cp.canceled ? "Compare canceled" : "Comparing…"} onClose={close} width={560}>
      <div className="compare-sources">
        <div>
          <span className="muted">Source</span>
          <code>{displaySpec(cp.to, profiles)}</code>
          {cp.toCount !== undefined && <span className="count">{cp.toCount} objects</span>}
        </div>
        <div>
          <span className="muted">Target</span>
          <code>{displaySpec(cp.from, profiles)}</code>
          {cp.fromCount !== undefined && <span className="count">{cp.fromCount} objects</span>}
        </div>
      </div>

      <div className="compare-phase" role="status" aria-live="polite">
        {cp.running && <span className="spinner" aria-hidden="true" />}
        <span>{cp.message}</span>
      </div>

      {cp.steps.length > 0 && (
        <ul className="compare-steps">
          {cp.steps.map((s, i) => (
            <li key={i}>
              <Check size={13} aria-hidden="true" /> {s}
            </li>
          ))}
        </ul>
      )}

      <div className="dialog-actions">
        {cp.running ? (
          <button type="button" onClick={cancelCompare}>
            Cancel
          </button>
        ) : (
          <button type="button" className="primary" onClick={close}>
            Close
          </button>
        )}
      </div>
    </Dialog>
  );
}
