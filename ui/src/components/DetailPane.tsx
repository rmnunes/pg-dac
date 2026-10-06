import { useState } from "react";
import { DiffEditor, Editor } from "@monaco-editor/react";

import { useStore } from "../state/store";

const monacoOpts = { readOnly: true, minimap: { enabled: false }, scrollBeyondLastLine: false, fontSize: 12 } as const;

export function DetailPane() {
  const { plan, selectedId } = useStore();
  const [tab, setTab] = useState<"diff" | "sql">("diff");
  const [showResult, setShowResult] = useState(false);

  const change = plan?.changes.find((c) => c.id === selectedId);
  if (!change) {
    return <div className="detail empty">Select an object to see its definition diff and generated SQL.</div>;
  }
  const sql = change.statements.map((s) => (s.note ? `-- ${s.note}\n` : "") + s.sql).join("\n\n");
  // The Target after the change is applied is the desired (Source) definition;
  // a drop leaves nothing.
  const result = change.newDefinition ?? "";

  return (
    <div className="detail">
      <div className="detail-tabs">
        <button className={tab === "diff" ? "active" : ""} onClick={() => setTab("diff")}>Definition</button>
        <button className={tab === "sql" ? "active" : ""} onClick={() => setTab("sql")}>Generated SQL</button>
        <span className="detail-title">
          <span className={"kind kind-" + change.changeKind}>{change.changeKind}</span> {change.objectType}{" "}
          {change.schema ? change.schema + "." : ""}{change.name}
        </span>
      </div>

      {change.hazards && change.hazards.length > 0 && (
        <div className="hazards">
          {change.hazards.map((h, i) => (
            <span key={i} className={"badge " + (h.severity === "error" ? "sev-error" : "sev-warn")} title={h.message}>
              {h.type}
            </span>
          ))}
        </div>
      )}

      {tab === "diff" && (
        <div className="diff-toolbar">
          <label className="diff-toggle" title="Show the Target as it will look once the change is applied">
            <input type="checkbox" checked={showResult} onChange={(e) => setShowResult(e.target.checked)} />
            Show result (with changes applied)
          </label>
          {!showResult && (
            <span className="diff-legend">
              <span className="kind-add">green = added to Target</span> ·{" "}
              <span className="kind-drop">red = removed from Target</span>
            </span>
          )}
        </div>
      )}

      {tab === "diff" && !showResult && (
        <div className="diff-sides">
          <span>Source (of truth)</span>
          <span>Target (current)</span>
        </div>
      )}

      <div className="editor">
        {tab === "sql" ? (
          <Editor language="sql" value={sql} options={monacoOpts} />
        ) : showResult ? (
          // The Target after the change is applied (the desired definition), as a
          // single clean pane — no diff highlighting.
          <Editor
            language="sql"
            value={
              result ||
              (change.changeKind === "drop"
                ? "-- (object is removed — it will no longer exist in the Target)"
                : "-- (no stored definition for this object — see the Generated SQL tab)")
            }
            options={monacoOpts}
          />
        ) : (
          // SSDT layout: original (left) = Source (of truth), modified (right) =
          // Target (current); Update pushes Source → Target. The pgdac-diff theme
          // colors Source-only lines green (added to Target) and Target-only lines
          // red (removed); renderIndicators:false hides the +/- gutter glyphs so
          // they can't contradict the colors.
          <DiffEditor
            language="sql"
            theme="pgdac-diff"
            original={change.newDefinition ?? ""}
            modified={change.oldDefinition ?? ""}
            options={{ ...monacoOpts, renderSideBySide: true, renderIndicators: false }}
          />
        )}
      </div>
    </div>
  );
}
