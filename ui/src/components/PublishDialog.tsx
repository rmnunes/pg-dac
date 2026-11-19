import { useMemo, useState } from "react";

import { useStore } from "../state/store";
import { displaySpec, specIsProject } from "../state/profiles";
import { hazardInfo, objectName } from "../vocab";
import { Dialog } from "./Dialog";

const MAX_NAMES = 4;

export function PublishDialog({ onClose, onOptions }: { onClose: () => void; onOptions: () => void }) {
  const s = useStore();
  const { plan, publish } = s;
  const isProject = specIsProject(s.from, s.profiles);
  const unit = isProject ? "file" : "statement";
  const errorCodes = useMemo(() => (plan?.hazardSummary ?? []).filter((h) => h.severity === "error"), [plan]);
  const warnCodes = useMemo(() => (plan?.hazardSummary ?? []).filter((h) => h.severity !== "error"), [plan]);
  const [acked, setAcked] = useState<Set<string>>(new Set());

  // The plan is already re-planned for the selection: what has statements runs.
  const included = (plan?.changes ?? []).filter((c) => c.statements.length > 0);
  const statements = plan?.statementCount ?? included.reduce((n, c) => n + c.statements.length, 0);
  const objectsWith = (code: string) => included.filter((c) => c.hazards?.some((h) => h.type === code)).map(objectName);

  const allAcked = errorCodes.every((h) => acked.has(h.code));
  const canApply = !publish.running && !publish.done && allAcked;
  const toggleAck = (code: string) =>
    setAcked((prev) => {
      const n = new Set(prev);
      n.has(code) ? n.delete(code) : n.add(code);
      return n;
    });

  const apply = () => {
    // The engine applies error-severity hazards only when explicitly allowed;
    // the acknowledgements above are that permission.
    s.set({ allowHazards: errorCodes.length > 0 });
    void s.startPublish();
  };

  const names = (list: string[]) => {
    const shown = list.slice(0, MAX_NAMES);
    const rest = list.length - shown.length;
    return (
      <>
        {shown.map((n, i) => (
          <span key={n}>
            {i > 0 ? ", " : ""}
            <code>{n}</code>
          </span>
        ))}
        {rest > 0 ? ` and ${rest} more` : ""}
      </>
    );
  };

  return (
    <Dialog title={isProject ? "Update Target project" : "Publish to Target"} onClose={onClose} dismissable={!publish.running} width={680}>
      {isProject ? (
        <p>
          Rewrites the files of the Target project <code>{displaySpec(s.from, s.profiles)}</code> so they describe the Source. Changed objects are replaced in
          place, new objects get a file at the project's stable path, and objects the Source lacks are removed only when "Drop objects not in the Source"
          is on. Objects defined by migration-style statements are refused with directions.
        </p>
      ) : (
        <p>
          Applies {statements} {unit}
          {statements === 1 ? "" : "s"} across {included.length} change{included.length === 1 ? "" : "s"} to the Target{" "}
          <code>{displaySpec(s.from, s.profiles)}</code>, updating it to match the Source. A failed publish can be resumed by comparing again.
        </p>
      )}

      {!isProject && (
        <div className="summary" aria-label="Deployment settings">
          <span className="badge">{s.online ? "Online, lock-safe" : "Offline"}</span>
          <span className="badge">{s.singleTransaction ? "Single transaction" : "Statement by statement"}</span>
          <span className={"badge" + (s.dropObjects ? " sev-error" : "")}>{s.dropObjects ? "Drops objects not in the Source" : "Keeps extra Target objects"}</span>
          <span className={"badge" + (s.allowDrift ? " sev-error" : "")}>{s.allowDrift ? "Drift allowed" : "Refuses drift"}</span>
          <span className="badge">{s.skipDeployScripts ? "Deploy scripts skipped" : "Runs deploy scripts"}</span>
          <button type="button" className="link small" onClick={onOptions} disabled={publish.running || publish.done}>
            Change…
          </button>
        </div>
      )}

      {errorCodes.length > 0 && (
        <div className="ack-group">
          <h3>Acknowledge before applying</h3>
          {errorCodes.map((h) => {
            const info = hazardInfo(h.code);
            const id = "ack-" + h.code;
            return (
              <div className="ack" key={h.code}>
                <input id={id} type="checkbox" checked={acked.has(h.code)} onChange={() => toggleAck(h.code)} disabled={publish.running || publish.done} />
                <label htmlFor={id} className="title danger">
                  {info.title}
                </label>
                <div className="objs">
                  {info.description} {h.count} object{h.count === 1 ? "" : "s"}: {names(objectsWith(h.code))}
                </div>
              </div>
            );
          })}
        </div>
      )}

      {warnCodes.length > 0 && (
        <div className="ack-group">
          <h3>Risks to be aware of</h3>
          {warnCodes.map((h) => {
            const info = hazardInfo(h.code);
            return (
              <div className="ack" key={h.code}>
                <span className="badge sev-warning">{info.short}</span>
                <span className="title">{info.title}</span>
                <div className="objs">
                  {h.count} object{h.count === 1 ? "" : "s"}: {names(objectsWith(h.code))}
                </div>
              </div>
            );
          })}
        </div>
      )}

      {(publish.running || publish.done || publish.error) && (
        <div className="publish-progress" aria-live="polite">
          <div className="progress-bar" role="progressbar" aria-valuemin={0} aria-valuemax={publish.total || 1} aria-valuenow={publish.applied} aria-label="Publish progress">
            <div className="progress-fill" style={{ width: publish.total ? `${(publish.applied / publish.total) * 100}%` : "0%" }} />
          </div>
          <pre className="publish-log">{publish.log.join("\n")}</pre>
          {publish.error && <div className="inline-error">{publish.error}</div>}
          {publish.done && (
            <div className="ok-banner">
              Applied {publish.applied} {unit}
              {publish.applied === 1 ? "" : "s"}.
            </div>
          )}
        </div>
      )}

      <div className="dialog-actions">
        {!canApply && !publish.running && !publish.done && errorCodes.length > 0 && (
          <span className="hint" role="status">
            Tick each acknowledgement to enable Apply.
          </span>
        )}
        <button type="button" onClick={onClose} disabled={publish.running}>
          {publish.done ? "Close" : "Cancel"}
        </button>
        <button type="button" className={errorCodes.length > 0 ? "danger-fill" : "primary"} disabled={!canApply} onClick={apply}>
          {publish.running ? "Applying…" : "Apply"}
        </button>
      </div>
    </Dialog>
  );
}
