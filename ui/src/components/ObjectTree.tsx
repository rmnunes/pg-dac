import { useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";

import { useStore } from "../state/store";
import type { ChangeKind, Severity } from "../ipc/types";

const kindGlyph = (k: ChangeKind) => (k === "add" ? "＋" : k === "drop" ? "－" : "～");
const sevClass = (sev?: Severity) =>
  sev === "error" ? "sev-error" : sev === "warning" ? "sev-warn" : "";

export function ObjectTree() {
  const { plan, excluded, autoExcluded, selectedId, select, toggleExclude } = useStore();
  const nameOf = (id: string) => {
    const c = plan?.changes.find((x) => x.id === id);
    return c ? `${c.objectType} ${c.schema ? c.schema + "." : ""}${c.name}` : id;
  };
  const [hazardsOnly, setHazardsOnly] = useState(false);
  const parentRef = useRef<HTMLDivElement>(null);

  // All hooks must run unconditionally (Rules of Hooks): compute `changes`
  // defensively and call useVirtualizer BEFORE any early return.
  const allChanges = plan?.changes ?? [];
  const changes = hazardsOnly ? allChanges.filter((c) => (c.hazards?.length ?? 0) > 0) : allChanges;

  const rv = useVirtualizer({
    count: changes.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 30,
    overscan: 14,
  });

  if (!plan) {
    return <div className="tree empty">Run a comparison to see object changes.</div>;
  }
  if (plan.changes.length === 0) {
    return <div className="tree empty">No differences.</div>;
  }

  return (
    <div className="tree">
      <div className="tree-toolbar">
        <span>
          {changes.length} change(s)
          {excluded.size + autoExcluded.size > 0 ? ` · ${excluded.size + autoExcluded.size} excluded` : ""}
          {autoExcluded.size > 0 ? ` (${autoExcluded.size} by dependency)` : ""}
        </span>
        <label>
          <input type="checkbox" checked={hazardsOnly} onChange={(e) => setHazardsOnly(e.target.checked)} /> hazards only
        </label>
      </div>
      <div className="tree-scroll" ref={parentRef}>
        <div style={{ height: rv.getTotalSize(), position: "relative", width: "100%" }}>
          {rv.getVirtualItems().map((vi) => {
            const c = changes[vi.index];
            const gatedBy = autoExcluded.get(c.id);
            const isExcluded = excluded.has(c.id) || gatedBy !== undefined;
            return (
              <div
                key={c.id}
                className={"tree-row" + (c.id === selectedId ? " selected" : "") + (isExcluded ? " excluded" : "")}
                style={{ position: "absolute", top: 0, left: 0, right: 0, height: vi.size, transform: `translateY(${vi.start}px)` }}
                onClick={() => select(c.id)}
                title={gatedBy ? `Excluded because it depends on ${nameOf(gatedBy)}, which is excluded` : undefined}
              >
                <input
                  type="checkbox"
                  checked={!isExcluded}
                  disabled={gatedBy !== undefined}
                  onClick={(e) => e.stopPropagation()}
                  onChange={() => toggleExclude(c.id)}
                />
                <span className={"kind kind-" + c.changeKind} title={c.changeKind}>{kindGlyph(c.changeKind)}</span>
                <span className="otype">{c.objectType}</span>
                <span className="oname">{c.schema ? c.schema + "." : ""}{c.name}</span>
                {c.severity ? <span className={"badge " + sevClass(c.severity)}>{c.severity}</span> : null}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
