import { useEffect, useState } from "react";

import { health, onCompare, onPublish } from "./ipc/client";
import { EXPECTED_FORMAT_VERSION } from "./ipc/types";
import pinnedVersion from "../PGDAC_VERSION?raw";
import { useStore } from "./state/store";
import { SourceBar } from "./components/SourceBar";
import { ObjectTree } from "./components/ObjectTree";
import { DetailPane } from "./components/DetailPane";
import { PublishModal } from "./components/PublishModal";
import { ScriptModal } from "./components/ScriptModal";
import { CompareProgress } from "./components/CompareProgress";
import { specIsDb, specIsProject } from "./state/profiles";

// Warnings arrive tagged with the side they concern; the GUI's names for the
// engine's --from / --to are Target / Source.
function sideLabel(side: string): string {
  switch (side) {
    case "target":
      return "Target";
    case "source":
      return "Source";
    case "plan":
      return "Plan";
    default:
      return side;
  }
}

export default function App() {
  const { from, profiles, plan, warnings, error, generateScript, loadProfiles, handlePublishEvent, handleCompareEvent } =
    useStore();
  const { treeWidth, setTreeWidth, persistTreeWidth } = useStore();
  const [showPublish, setShowPublish] = useState(false);
  const [scriptText, setScriptText] = useState<string | null>(null);

  // Draggable divider between the change list and the results. Updates the width
  // live during the drag and persists it once on release.
  const startResize = (e: React.MouseEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = treeWidth;
    const onMove = (ev: MouseEvent) => setTreeWidth(startW + (ev.clientX - startX));
    const onUp = () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
      document.body.classList.remove("resizing");
      persistTreeWidth();
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
    document.body.classList.add("resizing");
  };

  const [engineNote, setEngineNote] = useState<string | null>(null);

  useEffect(() => {
    const subs = [onPublish(handlePublishEvent), onCompare(handleCompareEvent)];
    void loadProfiles();
    // Handshake with the bundled engine: the plan format must be the one this
    // UI understands, and the sidecar should be the pinned version.
    health()
      .then((h) => {
        if (h.formatVersion !== EXPECTED_FORMAT_VERSION) {
          setEngineNote(`The pg-dac engine reports plan format ${h.formatVersion}; this UI expects ${EXPECTED_FORMAT_VERSION}. Rebuild the sidecar to match.`);
          return;
        }
        const pinned = pinnedVersion.trim();
        if (h.version.replace(/-dev$/, "") !== pinned) {
          setEngineNote(`Engine ${h.version} differs from the UI's pinned ${pinned}; results may differ from a release build.`);
        }
      })
      .catch((e) => setEngineNote(`pg-dac engine not reachable: ${String(e)}`));
    return () => {
      subs.forEach((un) => un.then((f) => f()));
    };
  }, [handlePublishEvent, handleCompareEvent, loadProfiles]);

  const hasChanges = !!plan && plan.changes.length > 0;
  // A Target is published to when it is a live database (the migration runs
  // there) or a project directory (its files are updated from the Source). An
  // artifact cannot be a Target. Handles profile refs too.
  const targetIsDb = specIsDb(from, profiles);
  const targetIsProject = specIsProject(from, profiles);
  const canPublish = targetIsDb || targetIsProject;
  const counts = (plan?.changes ?? []).reduce<Record<string, number>>((a, c) => {
    a[c.changeKind] = (a[c.changeKind] ?? 0) + 1;
    return a;
  }, {});

  return (
    <div className="app">
      <header className="titlebar">
        pg-dac <span className="sub">PostgreSQL schema compare</span>
      </header>

      <SourceBar />

      {plan && (
        <div className="direction-banner">
          Update <b>Target</b> to match <b>Source</b>
          {hasChanges ? (
            <span className="counts">
              {" — "}
              <span className="kind-add">{counts.add ?? 0} add</span>
              {" · "}
              <span className="kind-alter">{counts.alter ?? 0} change</span>
              {counts.drop ? (
                <>
                  {" · "}
                  <span className="kind-drop">{counts.drop} drop</span>
                </>
              ) : null}
            </span>
          ) : (
            " — in sync"
          )}
        </div>
      )}

      {warnings.length > 0 && (
        <div className="warnings">
          {warnings.map((w, i) => (
            <div key={i}>
              ⚠ {w.side && <b>{sideLabel(w.side)}: </b>}
              {w.message}
            </div>
          ))}
        </div>
      )}
      {engineNote && <div className="warnings"><div>⚠ {engineNote}</div></div>}
      {error && <div className="error-banner">{error}</div>}

      <div className="workspace">
        <div className="tree-pane" style={{ width: treeWidth }}>
          <ObjectTree />
        </div>
        <div
          className="splitter"
          onMouseDown={startResize}
          role="separator"
          aria-orientation="vertical"
          title="Drag to resize the change list"
        />
        <DetailPane />
      </div>

      <footer className="statusbar">
        <span>
          {plan
            ? `${plan.statementCount} statement(s)${plan.prelude?.length ? ` · ${plan.prelude.length} refactorlog rename(s)` : ""}${plan.dataLoss ? " · ⚠ DATA LOSS" : ""}`
            : "No comparison yet"}
        </span>
        <div className="actions">
          <button
            disabled={!hasChanges}
            onClick={async () => {
              const sql = await generateScript();
              if (sql) setScriptText(sql);
            }}
          >
            Generate Script
          </button>
          <button
            className="primary"
            disabled={!hasChanges || !canPublish}
            title={
              targetIsDb
                ? "Apply the changes to the Target database"
                : targetIsProject
                  ? "Rewrite the Target project's files so they describe the Source (SSDT's Update project from database)"
                  : "Publish updates the Target — set the Target to a database connection or a project directory"
            }
            onClick={() => setShowPublish(true)}
          >
            {targetIsProject ? "Update Target project…" : "Publish to Target…"}
          </button>
        </div>
      </footer>

      <CompareProgress />
      {showPublish && <PublishModal onClose={() => setShowPublish(false)} />}
      {scriptText !== null && <ScriptModal sql={scriptText} onClose={() => setScriptText(null)} />}
    </div>
  );
}
