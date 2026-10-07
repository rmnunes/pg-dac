import { useEffect, useState } from "react";
import { X } from "lucide-react";

import { health, onCompare, onPublish } from "./ipc/client";
import { EXPECTED_FORMAT_VERSION } from "./ipc/types";
import pinnedVersion from "../PGDAC_VERSION?raw";
import { TREE_WIDTH_MAX, TREE_WIDTH_MIN, useStore } from "./state/store";
import { SourceBar } from "./components/SourceBar";
import { Toolbar } from "./components/Toolbar";
import { ChangeList } from "./components/ChangeList";
import { DetailPane } from "./components/DetailPane";
import { NotesPanel } from "./components/NotesPanel";
import { StatusBar, type EngineInfo } from "./components/StatusBar";
import { PublishDialog } from "./components/PublishDialog";
import { ScriptDialog } from "./components/ScriptDialog";
import { OptionsDialog, type OptionsTab } from "./components/OptionsDialog";
import { CompareProgress } from "./components/CompareProgress";

export default function App() {
  const { plan, warnings, error, generateScript, loadProfiles, handlePublishEvent, handleCompareEvent, set } = useStore();
  const { treeWidth, setTreeWidth, persistTreeWidth } = useStore();
  const [showPublish, setShowPublish] = useState(false);
  const [scriptText, setScriptText] = useState<string | null>(null);
  const [optionsTab, setOptionsTab] = useState<OptionsTab | null>(null);
  const [notesOpen, setNotesOpen] = useState(true);
  const [engine, setEngine] = useState<EngineInfo>({});

  // Draggable divider between the change list and the results. Updates the width
  // live during the drag and persists it once on release. Arrow keys work too.
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
  const resizeByKey = (e: React.KeyboardEvent) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    setTreeWidth(treeWidth + (e.key === "ArrowRight" ? 16 : -16));
    persistTreeWidth();
  };

  useEffect(() => {
    const subs = [onPublish(handlePublishEvent), onCompare(handleCompareEvent)];
    void loadProfiles();
    // Handshake with the bundled engine: the plan format must be the one this
    // UI understands, and the sidecar should be the pinned version.
    health()
      .then((h) => {
        if (h.formatVersion !== EXPECTED_FORMAT_VERSION) {
          setEngine({ version: h.version, note: `The engine reports plan format ${h.formatVersion}; this UI expects ${EXPECTED_FORMAT_VERSION}. Rebuild the sidecar to match.` });
          return;
        }
        const pinned = pinnedVersion.trim();
        // A dev engine reports "<version>-dev+<commit>" (see ui/README.md); only
        // the release part has to match the pin.
        const mismatch = h.version.replace(/ \(.*\)$/, "").replace(/-dev(\+[0-9a-f]+)?$/, "") !== pinned;
        setEngine({ version: h.version, note: mismatch ? `Engine ${h.version} differs from the UI's pinned ${pinned}; results may differ from a release build.` : undefined });
      })
      .catch((e) => setEngine({ note: `pg-dac engine not reachable: ${String(e)}` }));
    return () => {
      subs.forEach((un) => un.then((f) => f()));
    };
  }, [handlePublishEvent, handleCompareEvent, loadProfiles]);

  // A fresh result re-opens the notes so new warnings are seen; the user can
  // collapse them again from the status bar.
  useEffect(() => {
    if (warnings.length > 0) setNotesOpen(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [plan]);

  // Ctrl+Enter compares (the toolbar button's shortcut).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === "Enter") {
        const s = useStore.getState();
        if (s.from && s.to && s.status !== "comparing" && !document.querySelector("dialog[open]")) {
          e.preventDefault();
          void s.runCompare();
        }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  return (
    <div className="app">
      <header className="appbar">
        <h1>
          pg-dac <span className="sub">PostgreSQL schema compare</span>
        </h1>
        <Toolbar
          onOptions={() => setOptionsTab("compare")}
          onScript={async () => {
            const sql = await generateScript();
            if (sql) setScriptText(sql);
          }}
          onPublish={() => setShowPublish(true)}
        />
      </header>

      <SourceBar />

      {error && (
        <div className="error-banner" role="alert">
          <span className="msg">{error}</span>
          <button type="button" className="icon-btn ghost" aria-label="Dismiss this error" onClick={() => set({ error: undefined, status: "idle" })}>
            <X size={14} aria-hidden="true" />
          </button>
        </div>
      )}

      <main className="workspace">
        <aside className="list-pane" style={{ width: treeWidth }} aria-label="Changes">
          <ChangeList />
        </aside>
        <div
          className="splitter"
          onMouseDown={startResize}
          onKeyDown={resizeByKey}
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize the change list"
          aria-valuemin={TREE_WIDTH_MIN}
          aria-valuemax={TREE_WIDTH_MAX}
          aria-valuenow={treeWidth}
          tabIndex={0}
          title="Drag or use the arrow keys to resize the change list"
        />
        <DetailPane />
      </main>

      <NotesPanel open={notesOpen} />
      <StatusBar notesOpen={notesOpen} onToggleNotes={() => setNotesOpen((o) => !o)} engine={engine} />

      <CompareProgress />
      {optionsTab && <OptionsDialog initialTab={optionsTab} onClose={() => setOptionsTab(null)} />}
      {showPublish && <PublishDialog onClose={() => setShowPublish(false)} onOptions={() => setOptionsTab("deploy")} />}
      {scriptText !== null && <ScriptDialog sql={scriptText} onClose={() => setScriptText(null)} />}
    </div>
  );
}
