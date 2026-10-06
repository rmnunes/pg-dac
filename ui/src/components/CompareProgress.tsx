import { useStore } from "../state/store";
import { displaySpec } from "../state/profiles";

// A modal shown while a compare runs: it narrates what the daemon is doing
// (reading each side, then diffing/planning) and offers a Cancel button that
// actually aborts the work on the daemon. It auto-closes when the result arrives;
// a canceled compare leaves a dismissible "Canceled." state.
export function CompareProgress() {
  const { compareProgress: cp, cancelCompare, set, profiles } = useStore();
  if (!cp.running && !cp.canceled) return null;

  return (
    <div className="modal-backdrop">
      <div className="modal compare-progress" onClick={(e) => e.stopPropagation()}>
        <h2>{cp.canceled ? "Compare canceled" : "Comparing…"}</h2>

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

        <div className="compare-phase">
          {cp.running && <span className="spinner" aria-hidden />}
          <span>{cp.message}</span>
        </div>

        {cp.steps.length > 0 && (
          <ul className="compare-steps">
            {cp.steps.map((s, i) => (
              <li key={i}>✓ {s}</li>
            ))}
          </ul>
        )}

        <div className="modal-actions">
          {cp.running ? (
            <button className="danger" onClick={cancelCompare}>
              Cancel
            </button>
          ) : (
            <button onClick={() => set({ compareProgress: { ...cp, canceled: false } })}>Close</button>
          )}
        </div>
      </div>
    </div>
  );
}
