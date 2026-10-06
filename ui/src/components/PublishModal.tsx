import { useMemo, useState } from "react";

import { useStore } from "../state/store";
import { displaySpec, specIsProject } from "../state/profiles";

export function PublishModal({ onClose }: { onClose: () => void }) {
  const { plan, publish, allowHazards, set, startPublish, from, profiles } = useStore();
  const isProject = specIsProject(from, profiles);
  const unit = isProject ? "file" : "statement";
  const errorCodes = useMemo(
    () => (plan?.hazardSummary ?? []).filter((h) => h.severity === "error"),
    [plan],
  );
  const warnCodes = useMemo(
    () => (plan?.hazardSummary ?? []).filter((h) => h.severity !== "error"),
    [plan],
  );
  const [acked, setAcked] = useState<Set<string>>(new Set());

  const allErrorsAcked = errorCodes.every((h) => acked.has(h.code));
  const dataLossOk = !plan?.dataLoss || allowHazards;
  const canApply = !publish.running && !publish.done && allErrorsAcked && dataLossOk;

  const toggleAck = (code: string) =>
    setAcked((prev) => {
      const n = new Set(prev);
      n.has(code) ? n.delete(code) : n.add(code);
      return n;
    });

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal publish" onClick={(e) => e.stopPropagation()}>
        <h2>{isProject ? "Update Target project" : "Publish to Target"}</h2>
        {isProject ? (
          <p className="muted">
            Rewrites the files of the <b>Target</b> project <code>{displaySpec(from, profiles)}</code> so they describe
            the Source: changed objects are replaced in place, new objects get a file at the project's stable path,
            and objects the Source lacks are removed only under drop policy <code>all</code>. Objects defined by
            migration-style statements are refused with directions.
          </p>
        ) : (
          <p className="muted">
            Applies the migration to the <b>Target</b> <code>{displaySpec(from, profiles)}</code>, updating it to match
            the Source. This is not all-or-nothing — CONCURRENTLY statements run outside a transaction; a failed
            publish can be resumed by comparing again.
          </p>
        )}

        {plan?.dataLoss && (
          <label className="ack danger">
            <input type="checkbox" checked={allowHazards} onChange={(e) => set({ allowHazards: e.target.checked })} />
            This migration can cause <b>DATA LOSS</b>. Allow it.
          </label>
        )}

        {errorCodes.length > 0 && (
          <div className="ack-group">
            <h3>Acknowledge before applying</h3>
            {errorCodes.map((h) => (
              <label key={h.code} className="ack danger">
                <input type="checkbox" checked={acked.has(h.code)} onChange={() => toggleAck(h.code)} />
                <span className="badge sev-error">{h.code}</span> affects {h.count} object(s)
              </label>
            ))}
          </div>
        )}

        {warnCodes.length > 0 && (
          <div className="ack-group">
            <h3>Warnings</h3>
            {warnCodes.map((h) => (
              <div key={h.code} className="ack">
                <span className="badge sev-warn">{h.code}</span> {h.count} object(s)
              </div>
            ))}
          </div>
        )}

        {(publish.running || publish.done || publish.error) && (
          <div className="publish-progress">
            <div className="progress-bar">
              <div className="progress-fill" style={{ width: publish.total ? `${(publish.applied / publish.total) * 100}%` : "0%" }} />
            </div>
            <pre className="publish-log">{publish.log.join("\n")}</pre>
            {publish.error && <div className="error-banner">{publish.error}</div>}
            {publish.done && <div className="ok-banner">Applied {publish.applied} {unit}(s).</div>}
          </div>
        )}

        <div className="modal-actions">
          <button onClick={onClose}>{publish.done ? "Close" : "Cancel"}</button>
          <button className="primary danger" disabled={!canApply} onClick={startPublish}>
            {publish.running ? "Applying…" : "Apply"}
          </button>
        </div>
      </div>
    </div>
  );
}
