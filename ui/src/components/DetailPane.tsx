import { useId, useState } from "react";
import { DiffEditor, Editor } from "@monaco-editor/react";
import { AlertTriangle, Info, OctagonAlert } from "lucide-react";

import type { ObjectChange } from "../ipc/types";

import { READ_ONLY_OPTIONS, THEME } from "../monaco-setup";
import { useStore } from "../state/store";
import { firstLine, hazardInfo, kindGlyph, kindLabel, objectLabel, objectName, plural, sortedHazards, typeLabel, unchangedText } from "../vocab";
import { TabList, panelId, tabId, type TabSpec } from "./TabList";

type Tab = "diff" | "sql" | "risks" | "deps";

export function DetailPane() {
  const plan = useStore((s) => s.plan);
  const selectedId = useStore((s) => s.selectedId);
  const excluded = useStore((s) => s.excluded);
  const autoExcluded = useStore((s) => s.autoExcluded);
  const select = useStore((s) => s.select);
  const [tab, setTab] = useState<Tab>("diff");
  const [showResult, setShowResult] = useState(false);
  const uid = useId();

  const change = plan?.changes.find((c) => c.id === selectedId);
  const empty = !change;
  const sql = !change
    ? ""
    : change.statements.length === 0
      ? "-- Nothing will run for this object under the current selection."
      : change.statements.map((s) => (s.note ? `-- ${s.note}\n` : "") + s.sql).join("\n\n");
  // The Target after the change is applied is the desired (Source) definition;
  // a drop leaves nothing.
  const result = change
    ? change.newDefinition ||
      (change.changeKind === "drop" ? "-- The object is removed: it will no longer exist in the Target." : "-- No stored definition for this object; see the Generated SQL tab.")
    : "";
  const hazards = change ? sortedHazards(change) : [];

  // Dependencies within this plan, both ways, from the plan's dependsOn edges.
  const all = plan?.changes ?? [];
  const byId = new Map(all.map((c) => [c.id, c]));
  const upstream = change ? (change.dependsOn ?? []).map((id) => byId.get(id)).filter((c): c is ObjectChange => !!c) : [];
  const downstream = change ? all.filter((c) => c.id !== change.id && (c.dependsOn ?? []).includes(change.id)) : [];
  const stateOf = (c: ObjectChange): { label: string; cls: string } => {
    const gate = autoExcluded.get(c.id);
    if (gate !== undefined) {
      const g = byId.get(gate);
      return { label: `excluded with ${g ? objectName(g) : gate}`, cls: "sev-info" };
    }
    if (excluded.has(c.id)) return c.statements.length > 0 ? { label: "excluded, still recreated", cls: "sev-warning" } : { label: "excluded", cls: "sev-info" };
    return { label: "included", cls: "ok" };
  };

  const tabs: TabSpec<Tab>[] = [
    { key: "diff", label: "Definition" },
    { key: "sql", label: "Generated SQL" },
    { key: "risks", label: "Risks", count: hazards.length },
    { key: "deps", label: "Dependencies", count: upstream.length + downstream.length },
  ];
  const depList = (items: ObjectChange[], empty: string) =>
    items.length === 0 ? (
      <p className="muted small">{empty}</p>
    ) : (
      <ul>
        {items.map((d) => {
          const st = stateOf(d);
          return (
            <li key={d.id}>
              <button
                type="button"
                className="link"
                title="Show this change"
                onClick={() => {
                  select(d.id);
                  // The link belongs to the previous object's list and goes
                  // away; keep the keyboard on the Dependencies tab.
                  requestAnimationFrame(() => document.getElementById(tabId(uid, "deps"))?.focus());
                }}
              >
                <span className={"glyph kind-" + d.changeKind} aria-hidden="true">
                  {kindGlyph(d.changeKind)}
                </span>{" "}
                {objectLabel(d)}
              </button>
              <span className={"badge " + st.cls}>{st.label}</span>
            </li>
          );
        })}
      </ul>
    );

  return (
    <section className="detail" aria-label="Change details">
      {empty ? (
        <div className="empty">Select a change to see its definition, the generated SQL and its risks.</div>
      ) : (
        <TabList base={uid} tabs={tabs} active={tab} onSelect={setTab} label="Detail views" className="tabs">
          <span className="detail-title">
            <b className={"kind-" + change.changeKind}>{kindLabel(change.changeKind)}</b> {typeLabel(change.objectType)} <b>{objectName(change)}</b>
          </span>
        </TabList>
      )}

      {!empty && tab === "diff" && unchangedText(change) && (
        <div className="info-strip" role="note">
          <Info className="icon" size={14} aria-hidden="true" />
          <span>
            <b>Definition unchanged.</b>{" "}
            {change.reason
              ? `This object is ${change.reason}.`
              : "The change is in an attribute the definition text does not show."}{" "}
            <button type="button" className="link" onClick={() => setTab("sql")}>
              See the {plural(change.statements.length, "statement")} that will run on the Target
            </button>
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
      <div className="editor-stack" id={panelId(uid, "diff")} role="tabpanel" aria-labelledby={tabId(uid, "diff")} hidden={empty || tab !== "diff"}>
        <div className="editor" hidden={showResult}>
          <DiffEditor
            language="sql"
            theme={THEME}
            original={change?.newDefinition ?? ""}
            modified={change?.oldDefinition ?? ""}
            options={{ ...READ_ONLY_OPTIONS, renderSideBySide: true, renderIndicators: false }}
          />
        </div>
        <div className="editor" hidden={!showResult}>
          <Editor language="sql" theme={THEME} value={result} options={READ_ONLY_OPTIONS} />
        </div>
      </div>
      <div className="editor" id={panelId(uid, "sql")} role="tabpanel" aria-labelledby={tabId(uid, "sql")} hidden={empty || tab !== "sql"}>
        <Editor language="sql" theme={THEME} value={sql} options={READ_ONLY_OPTIONS} />
      </div>

      {!empty && tab === "risks" && (
        <div className="risks" id={panelId(uid, "risks")} role="tabpanel" aria-labelledby={tabId(uid, "risks")}>
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

      {!empty && tab === "deps" && (
        <div className="deps" id={panelId(uid, "deps")} role="tabpanel" aria-labelledby={tabId(uid, "deps")}>
          <section>
            <h4>Depends on, in this plan ({upstream.length})</h4>
            {depList(upstream, "Nothing else in this plan. Objects already in the Target are not listed.")}
          </section>
          <section>
            <h4>Depended on by, in this plan ({downstream.length})</h4>
            {depList(downstream, "Nothing in this plan depends on it.")}
          </section>
          <p className="muted small">
            Excluding this object also excludes everything listed under "depended on by"; including it brings back what it depends on. An
            excluded object the engine still has to recreate for a dependency is marked "still recreated".
          </p>
        </div>
      )}
    </section>
  );
}
