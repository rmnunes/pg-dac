import { useId, useMemo, useState } from "react";
import { useShallow } from "zustand/react/shallow";

import { optionsStale, useStore } from "../state/store";
import { isExcluded, runningChanges } from "../state/plan";
import { displaySpec, specIsProject } from "../state/profiles";
import { hazardInfo, objectLabel, objectName, plural } from "../vocab";
import { Dialog } from "./Dialog";

const MAX_NAMES = 4;

export function PublishDialog({ onClose, onOptions }: { onClose: () => void; onOptions: () => void }) {
  const plan = useStore((s) => s.plan);
  const publish = useStore((s) => s.publish);
  const from = useStore((s) => s.from);
  const profiles = useStore((s) => s.profiles);
  const excluded = useStore((s) => s.excluded);
  const autoExcluded = useStore((s) => s.autoExcluded);
  const replanning = useStore((s) => s.replanning);
  const replanError = useStore((s) => s.replanError);
  const stale = useStore(optionsStale);
  const settings = useStore(
    useShallow((s) => ({ online: s.online, singleTransaction: s.singleTransaction, dropObjects: s.dropObjects, allowDrift: s.allowDrift, skipDeployScripts: s.skipDeployScripts })),
  );
  const startPublish = useStore((s) => s.startPublish);
  const uid = useId();

  const isProject = specIsProject(from, profiles);
  const unit = isProject ? "file" : "statement";
  const errorCodes = useMemo(() => (plan?.hazardSummary ?? []).filter((h) => h.severity === "error"), [plan]);
  const warnCodes = useMemo(() => (plan?.hazardSummary ?? []).filter((h) => h.severity !== "error"), [plan]);
  const [acked, setAcked] = useState<Set<string>>(new Set());

  // The plan is re-planned for the selection: what has statements runs.
  const included = runningChanges(plan);
  const statements = plan?.statementCount ?? 0;
  const objectsWith = (code: string) => included.filter((c) => c.hazards?.some((h) => h.type === code)).map(objectName);
  // Excluded by the user, yet the engine must drop and recreate them (a
  // dependency changes): worth seeing before Apply.
  const forced = included.filter((c) => isExcluded(c.id, excluded, autoExcluded));

  // Apply runs exactly what the dialog shows: never while the selection's
  // re-plan is pending or failed, nor with options changed since the compare.
  const blocker = replanning
    ? "Updating the plan for the current selection…"
    : replanError
      ? "The plan could not be updated for the current selection; compare again."
      : stale
        ? "Options or sources changed since the comparison; compare again before publishing."
        : undefined;
  const allAcked = errorCodes.every((h) => acked.has(h.code));
  const canApply = !publish.running && !publish.done && allAcked && !blocker;
  const toggleAck = (code: string) =>
    setAcked((prev) => {
      const n = new Set(prev);
      if (n.has(code)) n.delete(code);
      else n.add(code);
      return n;
    });

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
          Rewrites the files of the Target project <code>{displaySpec(from, profiles)}</code> so they describe the Source. Changed objects are replaced in
          place, new objects get a file at the project's stable path, and objects the Source lacks are removed only when "Drop objects not in the Source"
          is on. Objects defined by migration-style statements are refused with directions.
        </p>
      ) : (
        <p>
          Applies {plural(statements, unit)} across {plural(included.length, "change")} to the Target <code>{displaySpec(from, profiles)}</code>, updating it
          to match the Source. A failed publish can be resumed by comparing again.
        </p>
      )}

      {!isProject && (
        <div className="summary" aria-label="Deployment settings">
          <span className="badge">{settings.online ? "Online, lock-safe" : "Offline"}</span>
          <span className="badge">{settings.singleTransaction ? "Single transaction" : "Statement by statement"}</span>
          <span className={"badge" + (settings.dropObjects ? " sev-error" : "")}>{settings.dropObjects ? "Drops objects not in the Source" : "Keeps extra Target objects"}</span>
          <span className={"badge" + (settings.allowDrift ? " sev-error" : "")}>{settings.allowDrift ? "Drift allowed" : "Refuses drift"}</span>
          <span className="badge">{settings.skipDeployScripts ? "Deploy scripts skipped" : "Runs deploy scripts"}</span>
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
            const id = `${uid}-ack-${h.code}`;
            return (
              <div className="ack" key={h.code}>
                <input id={id} type="checkbox" checked={acked.has(h.code)} onChange={() => toggleAck(h.code)} disabled={publish.running || publish.done} />
                <label htmlFor={id} className="title danger">
                  {info.title}
                </label>
                <div className="objs">
                  {info.description} {plural(h.count, "object")}: {names(objectsWith(h.code))}
                </div>
              </div>
            );
          })}
        </div>
      )}

      {forced.length > 0 && (
        <div className="ack-group">
          <h3>Excluded, but still touched for a dependency</h3>
          {forced.map((c) => (
            <div className="ack" key={c.id}>
              <span className="badge sev-warning">recreated</span>
              <span className="title">{objectLabel(c)}</span>
              <div className="objs">{c.reason || "see its Generated SQL tab"}</div>
            </div>
          ))}
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
                  {plural(h.count, "object")}: {names(objectsWith(h.code))}
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
          {publish.done && <div className="ok-banner">Applied {plural(publish.applied, unit)}.</div>}
        </div>
      )}

      <div className="dialog-actions">
        {!publish.running && !publish.done && (blocker || (!allAcked && errorCodes.length > 0)) && (
          <span className="hint" role="status">
            {blocker ?? "Tick each acknowledgement to enable Apply."}
          </span>
        )}
        <button type="button" onClick={onClose} disabled={publish.running}>
          {publish.done ? "Close" : "Cancel"}
        </button>
        <button type="button" className={errorCodes.length > 0 ? "danger-fill" : "primary"} disabled={!canApply} onClick={() => void startPublish(errorCodes.length > 0)}>
          {publish.running ? "Applying…" : "Apply"}
        </button>
      </div>
    </Dialog>
  );
}
