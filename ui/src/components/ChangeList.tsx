import { useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ChevronDown, ChevronRight, Search } from "lucide-react";

import { useStore } from "../state/store";
import { isExcluded } from "../state/plan";
import type { ChangeKind, ObjectChange } from "../ipc/types";
import { KIND_ORDER, gatedTitle, hazardInfo, kindGlyph, kindLabel, objectLabel, objectName, plural, sortedHazards, topHazard, typeLabel, unchangedText } from "../vocab";

// The change list, grouped by action (Drop / Change / Add) like SSDT's results
// grid. Groups collapse, carry a tri-state include checkbox, and the whole list
// is one keyboard-navigable tree (arrow keys move, Space includes/excludes,
// Enter selects, Left/Right collapse/expand). Rows are virtualized over the
// flattened group+item sequence.
type Row =
  | { kind: "group"; group: ChangeKind; items: ObjectChange[]; included: number }
  | { kind: "item"; change: ObjectChange; group: ChangeKind; pos: number; size: number };

export function ChangeList() {
  const plan = useStore((s) => s.plan);
  const basePlan = useStore((s) => s.basePlan);
  const excluded = useStore((s) => s.excluded);
  const autoExcluded = useStore((s) => s.autoExcluded);
  const selectedId = useStore((s) => s.selectedId);
  const gatingNotice = useStore((s) => s.gatingNotice);
  const replanError = useStore((s) => s.replanError);
  const { select, toggleExclude, setIncluded, clearGatingNotice, runCompare } = useStore.getState();
  const [collapsed, setCollapsed] = useState<Set<ChangeKind>>(new Set());
  const [filter, setFilter] = useState("");
  const [risksOnly, setRisksOnly] = useState(false);
  const [active, setActive] = useState(0);
  const wantFocus = useRef(false);
  const parentRef = useRef<HTMLDivElement>(null);

  // A new comparison starts the list fresh; a re-plan (selection change) keeps
  // the filter, the collapsed groups and the keyboard position.
  useEffect(() => {
    setCollapsed(new Set());
    setFilter("");
    setRisksOnly(false);
    setActive(0);
  }, [basePlan]);

  const rows = useMemo<Row[]>(() => {
    const all = plan?.changes ?? [];
    const q = filter.trim().toLowerCase();
    const shown = all.filter((c) => {
      if (risksOnly && !(c.hazards?.length ?? 0)) return false;
      if (q && !(objectName(c).toLowerCase().includes(q) || typeLabel(c.objectType).toLowerCase().includes(q))) return false;
      return true;
    });
    const out: Row[] = [];
    for (const g of KIND_ORDER) {
      const items = shown.filter((c) => c.changeKind === g);
      if (!items.length) continue;
      out.push({ kind: "group", group: g, items, included: items.filter((c) => !isExcluded(c.id, excluded, autoExcluded)).length });
      if (!collapsed.has(g)) items.forEach((c, i) => out.push({ kind: "item", change: c, group: g, pos: i + 1, size: items.length }));
    }
    return out;
  }, [plan, filter, risksOnly, collapsed, excluded, autoExcluded]);

  const rv = useVirtualizer({
    count: rows.length,
    getScrollElement: () => parentRef.current,
    estimateSize: (i) => (rows[i]?.kind === "group" ? 30 : 28),
    overscan: 14,
  });

  // A selection made elsewhere (the Dependencies tab's jumps) moves the
  // keyboard position to that row and scrolls it into view, without taking
  // focus from where the user clicked.
  const lastSelected = useRef(selectedId);
  useEffect(() => {
    if (selectedId === lastSelected.current) return;
    lastSelected.current = selectedId;
    const i = rows.findIndex((r) => r.kind === "item" && r.change.id === selectedId);
    if (i >= 0 && i !== active) {
      setActive(i);
      rv.scrollToIndex(i, { align: "auto" });
    }
  }, [selectedId, rows, active, rv]);

  // After a keyboard move, focus the row once it is rendered (it may need a
  // scroll first, hence the second attempt on the next frame).
  useEffect(() => {
    if (!wantFocus.current) return;
    wantFocus.current = false;
    const focusRow = () => parentRef.current?.querySelector<HTMLElement>(`[data-index="${active}"]`)?.focus();
    focusRow();
    requestAnimationFrame(focusRow);
  }, [active, rows]);

  const moveTo = (i: number) => {
    const n = Math.max(0, Math.min(rows.length - 1, i));
    wantFocus.current = true;
    setActive(n);
    rv.scrollToIndex(n);
  };
  const toggleGroup = (g: ChangeKind) =>
    setCollapsed((prev) => {
      const n = new Set(prev);
      if (n.has(g)) n.delete(g);
      else n.add(g);
      return n;
    });

  const onKeyDown = (e: React.KeyboardEvent) => {
    const row = rows[active];
    if (!row) return;
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        moveTo(active + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        moveTo(active - 1);
        break;
      case "Home":
        e.preventDefault();
        moveTo(0);
        break;
      case "End":
        e.preventDefault();
        moveTo(rows.length - 1);
        break;
      case "Enter":
        e.preventDefault();
        if (row.kind === "item") select(row.change.id);
        else toggleGroup(row.group);
        break;
      case " ":
        e.preventDefault();
        if (row.kind === "item") {
          if (!autoExcluded.has(row.change.id)) toggleExclude(row.change.id);
        } else setIncluded(row.items.map((c) => c.id), row.included < row.items.length);
        break;
      case "ArrowRight":
        if (row.kind === "group" && collapsed.has(row.group)) {
          e.preventDefault();
          toggleGroup(row.group);
        }
        break;
      case "ArrowLeft":
        e.preventDefault();
        if (row.kind === "group") {
          if (!collapsed.has(row.group)) toggleGroup(row.group);
        } else {
          moveTo(rows.findIndex((r) => r.kind === "group" && r.group === row.group));
        }
        break;
    }
  };

  if (!plan) {
    return (
      <div className="empty">
        <h3>No comparison yet</h3>
        <p className="small">
          Choose a Source and a Target, then press Compare (<kbd>Ctrl</kbd>+<kbd>Enter</kbd>).
        </p>
      </div>
    );
  }
  if (plan.changes.length === 0) {
    return (
      <div className="empty">
        <h3>Target already matches the Source</h3>
        <p className="small">Nothing to publish.</p>
      </div>
    );
  }

  const byId = new Map(plan.changes.map((c) => [c.id, c]));
  const total = plan.changes.length;
  const shownCount = rows.reduce((n, r) => n + (r.kind === "group" ? r.items.length : 0), 0);
  const excludedCount = plan.changes.filter((c) => isExcluded(c.id, excluded, autoExcluded)).length;

  return (
    <>
      <div className="list-toolbar">
        <div className="filter">
          <Search className="lead" size={14} aria-hidden="true" />
          <input type="search" aria-label="Filter changes by name or type" placeholder="Filter changes" value={filter} onChange={(e) => setFilter(e.target.value)} />
        </div>
        <label>
          <input type="checkbox" checked={risksOnly} onChange={(e) => setRisksOnly(e.target.checked)} />
          Risks only
        </label>
      </div>
      <div className="list-summary" role="status">
        {shownCount === total ? plural(total, "change") : `${shownCount} of ${plural(total, "change")}`}
        {excludedCount > 0 ? ` · ${excludedCount} excluded` : ""}
        {autoExcluded.size > 0 ? ` (${autoExcluded.size} by dependency)` : ""}
      </div>
      {replanError && (
        <div className="list-notice warn" role="alert">
          <span>The plan could not be updated for this selection, so the counts and statements shown are the previous selection's. {replanError}</span>
          <button type="button" className="link" onClick={() => void runCompare()}>
            Compare again
          </button>
        </div>
      )}
      {gatingNotice && (
        <div className="list-notice" role="status">
          <span>{gatingNotice.text}</span>
          <button type="button" className="link" onClick={clearGatingNotice}>
            Dismiss
          </button>
        </div>
      )}
      <div className="list-scroll" ref={parentRef} role="tree" aria-label="Changes" onKeyDown={onKeyDown}>
        {rows.length === 0 ? (
          <div className="empty small">No changes match the filter.</div>
        ) : (
          <div style={{ height: rv.getTotalSize(), position: "relative", width: "100%" }}>
            {rv.getVirtualItems().map((vi) => {
              const r = rows[vi.index];
              const style: React.CSSProperties = { position: "absolute", top: 0, left: 0, right: 0, height: vi.size, transform: `translateY(${vi.start}px)` };
              const tabIndex = vi.index === active ? 0 : -1;
              if (r.kind === "group") {
                const open = !collapsed.has(r.group);
                const all = r.included === r.items.length;
                return (
                  <div
                    key={"g:" + r.group}
                    data-index={vi.index}
                    className="grp-row"
                    style={style}
                    role="treeitem"
                    aria-level={1}
                    aria-expanded={open}
                    aria-selected={false}
                    tabIndex={tabIndex}
                    onClick={() => {
                      setActive(vi.index);
                      toggleGroup(r.group);
                    }}
                    onFocus={() => setActive(vi.index)}
                  >
                    {open ? <ChevronDown className="caret" size={14} aria-hidden="true" /> : <ChevronRight className="caret" size={14} aria-hidden="true" />}
                    <input
                      type="checkbox"
                      tabIndex={-1}
                      checked={all}
                      ref={(el) => {
                        if (el) el.indeterminate = r.included > 0 && !all;
                      }}
                      aria-label={`Include all ${kindLabel(r.group).toLowerCase()} changes`}
                      onClick={(e) => e.stopPropagation()}
                      onChange={() => setIncluded(r.items.map((c) => c.id), !all)}
                    />
                    <span className={"glyph kind-" + r.group} aria-hidden="true">
                      {kindGlyph(r.group)}
                    </span>
                    {kindLabel(r.group)}
                    <span className="grp-count">({r.items.length})</span>
                    {r.included < r.items.length && <span className="grp-sub">{r.included} included</span>}
                  </div>
                );
              }
              const c = r.change;
              const gatedBy = autoExcluded.get(c.id);
              const gate = gatedBy !== undefined ? byId.get(gatedBy) : undefined;
              const gateName = gate ? objectName(gate) : gatedBy;
              const off = isExcluded(c.id, excluded, autoExcluded);
              const top = topHazard(c);
              const more = (c.hazards?.length ?? 0) - 1;
              return (
                <div
                  key={c.id}
                  data-index={vi.index}
                  className={"chg-row" + (c.id === selectedId ? " selected" : "") + (off ? " excluded" : "")}
                  style={style}
                  role="treeitem"
                  aria-level={2}
                  aria-posinset={r.pos}
                  aria-setsize={r.size}
                  aria-selected={c.id === selectedId}
                  tabIndex={tabIndex}
                  title={gateName ? gatedTitle(gateName) : undefined}
                  onClick={() => {
                    setActive(vi.index);
                    select(c.id);
                  }}
                  onFocus={() => setActive(vi.index)}
                >
                  <input
                    type="checkbox"
                    tabIndex={-1}
                    checked={!off}
                    disabled={gatedBy !== undefined}
                    aria-label={`Include ${objectLabel(c)}`}
                    onClick={(e) => e.stopPropagation()}
                    onChange={() => toggleExclude(c.id)}
                  />
                  <span className={"glyph kind-" + c.changeKind} aria-hidden="true">
                    {kindGlyph(c.changeKind)}
                  </span>
                  <span className="otype">{typeLabel(c.objectType)}</span>
                  <span className="oname">
                    {c.schema ? <span className="schema">{c.schema}.</span> : null}
                    {c.name}
                  </span>
                  {gateName && (
                    <span className="badge sev-info" title={gatedTitle(gateName)}>
                      needs {gateName}
                    </span>
                  )}
                  {unchangedText(c) && (
                    <span className="badge sev-info" title={c.reason || "The definition text is unchanged; see the Generated SQL tab"}>
                      {c.reason ? "recreated" : "no text change"}
                    </span>
                  )}
                  {top && (
                    <span className={"badge sev-" + (top.severity || "info")} title={hazardInfo(top.type).title}>
                      {hazardInfo(top.type).short}
                    </span>
                  )}
                  {more > 0 && (
                    <span className="badge sev-info" title={"Also: " + sortedHazards(c).slice(1).map((h) => hazardInfo(h.type).title).join(", ")}>
                      +{plural(more, "risk")}
                    </span>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </div>
    </>
  );
}
