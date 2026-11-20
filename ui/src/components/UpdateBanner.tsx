import type { ReactNode } from "react";
import { AlertTriangle, ArrowUpCircle, CheckCircle2, Download, RotateCw, X } from "lucide-react";

import { useStore } from "../state/store";
import { useUpdates } from "../state/update";

const fmtBytes = (n: number): string => (n < 1024 * 1024 ? `${Math.round(n / 1024)} KB` : `${(n / (1024 * 1024)).toFixed(1)} MB`);

/**
 * The update strip under the toolbar. It appears when a newer version is
 * found (or after a manual check), and the user decides when to download and
 * when to restart. Restarting stops the engine, so it waits while a compare,
 * script or publish is running.
 */
export function UpdateBanner() {
  const phase = useUpdates((s) => s.phase);
  const download = useUpdates((s) => s.download);
  const install = useUpdates((s) => s.install);
  const dismiss = useUpdates((s) => s.dismiss);
  const busy = useStore((s) => s.publish.running || s.compareProgress.running || s.status !== "idle");

  if (phase.kind === "idle" || (phase.kind === "checking" && !phase.manual)) return null;

  const later = (
    <button type="button" className="icon-btn ghost" aria-label="Hide until the next check" title="Later" onClick={dismiss}>
      <X size={14} aria-hidden="true" />
    </button>
  );

  let tone = "info";
  let icon = <ArrowUpCircle size={16} aria-hidden="true" />;
  let text: ReactNode;
  // Changes often (download progress), so it stays outside the live region.
  let detail: ReactNode = null;
  let actions: ReactNode = null;

  switch (phase.kind) {
    case "checking":
      icon = <RotateCw size={16} aria-hidden="true" className="spin" />;
      text = "Checking for updates…";
      break;
    case "available":
      text = (
        <>
          <strong>pg-dac {phase.info.version} is available.</strong> You have {phase.info.currentVersion}.
          {phase.error && <span className="update-error"> The download failed: {phase.error}</span>}
        </>
      );
      actions = (
        <>
          <button type="button" onClick={() => void download()}>
            <Download size={14} aria-hidden="true" />
            {phase.error ? "Try again" : "Download"}
          </button>
          {later}
        </>
      );
      break;
    case "downloading":
      icon = <Download size={16} aria-hidden="true" />;
      text = <strong>Downloading pg-dac {phase.info.version}…</strong>;
      detail = (
        <span className="update-detail">
          {fmtBytes(phase.received)}
          {phase.total ? ` of ${fmtBytes(phase.total)}` : ""}
        </span>
      );
      actions = <progress aria-label={`Downloading pg-dac ${phase.info.version}`} max={phase.total ?? undefined} value={phase.total ? phase.received : undefined} />;
      break;
    case "ready":
      tone = phase.error ? "warn" : "ok";
      icon = <CheckCircle2 size={16} aria-hidden="true" />;
      text = (
        <>
          <strong>pg-dac {phase.info.version} is ready.</strong> Restart to finish the update.
          {phase.error && <span className="update-error"> {phase.error}</span>}
          {busy && <span className="muted"> You can restart once the current operation finishes.</span>}
        </>
      );
      actions = (
        <>
          <button type="button" disabled={busy} onClick={() => void install()}>
            <RotateCw size={14} aria-hidden="true" />
            Restart now
          </button>
          {later}
        </>
      );
      break;
    case "installing":
      tone = "ok";
      icon = <RotateCw size={16} aria-hidden="true" className="spin" />;
      text = <strong>Installing pg-dac {phase.info.version}. The app restarts on its own.</strong>;
      break;
    case "uptodate":
      tone = "ok";
      icon = <CheckCircle2 size={16} aria-hidden="true" />;
      text = "pg-dac is up to date.";
      actions = later;
      break;
    case "failed":
      tone = "warn";
      icon = <AlertTriangle size={16} aria-hidden="true" />;
      text = <>Couldn't check for updates: {phase.message}</>;
      actions = later;
      break;
  }

  return (
    <div className={`update-banner ${tone}`} role="region" aria-label="Update">
      {icon}
      <span className="update-text" role="status">
        {text}
      </span>
      {detail}
      {actions && <span className="update-actions">{actions}</span>}
    </div>
  );
}
