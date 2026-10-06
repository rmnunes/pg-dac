import { useEffect, useState } from "react";

import { onCompare, onPublish } from "./ipc/client";
import { useStore } from "./state/store";
import { SourceBar } from "./components/SourceBar";
import { ObjectTree } from "./components/ObjectTree";
import { DetailPane } from "./components/DetailPane";
import { PublishModal } from "./components/PublishModal";
import { ScriptModal } from "./components/ScriptModal";
import { CompareProgress } from "./components/CompareProgress";
import { specIsDb } from "./state/profiles";

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

  useEffect(() => {
    const subs = [onPublish(handlePublishEvent), onCompare(handleCompareEvent)];
    void loadProfiles();
    return () => {
      subs.forEach((un) => un.then((f) => f()));
    };
  }, [handlePublishEvent, handleCompareEvent, loadProfiles]);

  const hasChanges = !!plan && plan.changes.length > 0;
  // A Target can only be published to if it's a live database (you can't apply a
  // migration to a project folder or an artifact). Handles profile refs too.
  const targetIsDb = specIsDb(from, profiles);
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
            <div key={i}>⚠ {w}</div>
          ))}
        </div>
      )}
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
            ? `${plan.statementCount} statement(s)${plan.dataLoss ? " · ⚠ DATA LOSS" : ""}`
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
            disabled={!hasChanges || !targetIsDb}
            title={
              targetIsDb
                ? "Apply the changes to the Target database"
                : "Publish updates the Target — set the Target to a database connection (or use Generate Script to back-port into a project)"
            }
            onClick={() => setShowPublish(true)}
          >
            Publish to Target…
          </button>
        </div>
      </footer>

      <CompareProgress />
      {showPublish && <PublishModal onClose={() => setShowPublish(false)} />}
      {scriptText !== null && <ScriptModal sql={scriptText} onClose={() => setScriptText(null)} />}
    </div>
  );
}
