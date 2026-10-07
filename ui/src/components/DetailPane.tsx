import { useId, useState } from "react";
import { DiffEditor, Editor } from "@monaco-editor/react";
import { AlertTriangle, Info, OctagonAlert } from "lucide-react";

import { THEME } from "../monaco-setup";
import { useStore } from "../state/store";
import { hazardInfo, kindLabel, objectName, sortedHazards } from "../vocab";

// automaticLayout: the editors stay mounted while hidden (switching tabs or
// changes must not dispose Monaco models — the DiffEditor wrapper disposes
// them in an order Monaco rejects), so they re-measure themselves when shown.
const monacoOpts = { readOnly: true, minimap: { enabled: false }, scrollBeyondLastLine: false, fontSize: 13, automaticLayout: true } as const;

type Tab = "diff" | "sql" | "risks";

const firstLine = (sql: string): string => {
  const i = sql.indexOf("\n");
  return i >= 0 ? sql.slice(0, i) + " …" : sql;
};

export function DetailPane() {
  const { plan, selectedId } = useStore();
  const [tab, setTab] = useState<Tab>("diff");
  const [showResult, setShowResult] = useState(false);
  const uid = useId();

  const change = plan?.changes.find((c) => c.id === selectedId);
  const empty = !change;
  const sql = change ? change.statements.map((s) => (s.note ? `-- ${s.note}\n` : "") + s.sql).join("\n\n") : "";
  // The Target after the change is applied is the desired (Source) definition;
  // a drop leaves nothing.
  const result = change
    ? change.newDefinition ||
      (change.changeKind === "drop" ? "-- The object is removed: it will no longer exist in the Target." : "-- No stored definition for this object; see the Generated SQL tab.")
    : "";
  const hazards = change ? sortedHazards(change) : [];

  const tabs: { key: Tab; label: string; count?: number }[] = [
    { key: "diff", label: "Definition" },
    { key: "sql", label: "Generated SQL" },
    { key: "risks", label: "Risks", count: hazards.length },
  ];
  const onTabKey = (e: React.KeyboardEvent) => {
    const i = tabs.findIndex((t) => t.key === tab);
    if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
      e.preventDefault();
      const n = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length].key;
      setTab(n);
      document.getElementById(`${uid}-tab-${n}`)?.focus();
    }
  };

  return (
    <section className="detail" aria-label="Change details">
      {empty ? (
        <div className="empty">Select a change to see its definition, the generated SQL and its risks.</div>
      ) : (
        <div className="tabs" role="tablist" aria-label="Detail views" onKeyDown={onTabKey}>
          {tabs.map((t) => (
            <button
              key={t.key}
              type="button"
              id={`${uid}-tab-${t.key}`}
              role="tab"
              className="tab"
              aria-selected={tab === t.key}
              aria-controls={`${uid}-panel-${t.key}`}
              tabIndex={tab === t.key ? 0 : -1}
              onClick={() => setTab(t.key)}
            >
              {t.label}
              {t.count ? <span className="count-pill">{t.count}</span> : null}
            </button>
          ))}
          <span className="detail-title">
            <b className={"kind-" + change.changeKind}>{kindLabel(change.changeKind)}</b> {change.objectType} <b>{objectName(change)}</b>
          </span>
        </div>
      )}

      {!empty && tab === "diff" && (
        <>
          <div className="diff-toolbar">
            <label className="diff-toggle" title="Show the Target as it will look once the change is applied">
              <input type="checkbox" checked={showResult} onChange={(e) => setShowResult(e.target.checked)} />
              Show the result (Target after the change)
            </label>
            {!showResult && (
              <span className="diff-legend">
                <b className="kind-add">green</b> is added to the Target · <b className="kind-drop">red</b> is removed from it
              </span>
            )}
          </div>
          {!showResult && (
            <div className="diff-sides" aria-hidden="true">
              <span>Source (of truth)</span>
              <span>Target (current)</span>
            </div>
          )}
        </>
      )}

      {/* SSDT layout: original (left) = Source (of truth), modified (right) =
          Target (current). The editor theme colours Source-only lines green
          (added to the Target) and Target-only lines red (removed);
          renderIndicators:false hides the +/- glyphs so they can't contradict
          the colours. */}
      <div className="editor" id={`${uid}-panel-diff`} role="tabpanel" aria-labelledby={`${uid}-tab-diff`} hidden={empty || tab !== "diff" || showResult}>
        <DiffEditor
          language="sql"
          theme={THEME}
          original={change?.newDefinition ?? ""}
          modified={change?.oldDefinition ?? ""}
          options={{ ...monacoOpts, renderSideBySide: true, renderIndicators: false }}
        />
      </div>
      <div className="editor" hidden={empty || tab !== "diff" || !showResult}>
        <Editor language="sql" theme={THEME} value={result} options={monacoOpts} />
      </div>
      <div className="editor" id={`${uid}-panel-sql`} role="tabpanel" aria-labelledby={`${uid}-tab-sql`} hidden={empty || tab !== "sql"}>
        <Editor language="sql" theme={THEME} value={sql} options={monacoOpts} />
      </div>

      {!empty && tab === "risks" && (
        <div className="risks" id={`${uid}-panel-risks`} role="tabpanel" aria-labelledby={`${uid}-tab-risks`}>
          {hazards.length === 0 && <div className="muted">No risks were identified for this change.</div>}
          {hazards.map((h) => {
            const info = hazardInfo(h.type);
            const stmts = change.statements.filter((s) => s.hazards?.some((x) => x.type === h.type));
            const sev = h.severity || "info";
            const Icon = sev === "error" ? OctagonAlert : sev === "warning" ? AlertTriangle : Info;
            return (
              <article className="risk" key={h.type}>
                <div className="risk-head">
                  <Icon className={"icon sev-" + sev} size={16} aria-hidden="true" />
                  <h4>{info.title}</h4>
                  <code title="hazard code">{h.type}</code>
                </div>
                <p>{h.message}</p>
                {info.description && <p className="muted small">{info.description}</p>}
                {stmts.length > 0 && (
                  <ul className="stmts" aria-label="Statements carrying this risk">
                    {stmts.map((s, i) => (
                      <li key={i} title={s.sql}>
                        {firstLine(s.sql)}
                        {s.note ? <span className="muted"> · {s.note}</span> : null}
                      </li>
                    ))}
                  </ul>
                )}
              </article>
            );
          })}
        </div>
      )}
    </section>
  );
}
