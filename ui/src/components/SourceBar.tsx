import { useStore } from "../state/store";
import { SourcePicker } from "./SourcePicker";

export function SourceBar() {
  const s = useStore();

  return (
    <div className="sourcebar">
      {/* SSDT Schema Compare layout: Source (of truth) on the LEFT, Target (gets
          updated) on the RIGHT; Update pushes Source → Target. Source is the
          engine's --to (desired); Target is --from (the side a publish applies to). */}
      <SourcePicker side="to" label="Source" hint="of truth" />

      <button className="swap" onClick={s.swap} title="swap Source and Target">⇄</button>

      <SourcePicker side="from" label="Target" hint="current · gets updated" />

      <details className="options">
        <summary>Options</summary>
        <div className="options-grid">
          <label>Schemas <input value={s.schemas} onChange={(e) => s.set({ schemas: e.target.value })} placeholder="app, audit" /></label>
          <label>Target PG major <input value={s.targetMajor} onChange={(e) => s.set({ targetMajor: e.target.value })} placeholder="16" /></label>
          <label><input type="checkbox" checked={s.online} onChange={(e) => s.set({ online: e.target.checked })} /> Online (lock-safe)</label>
          <label className="danger"><input type="checkbox" checked={s.dropObjects} onChange={(e) => s.set({ dropObjects: e.target.checked })} /> Drop objects not in source</label>
        </div>
      </details>

      <button className="primary compare" disabled={!s.from || !s.to || s.status === "comparing"} onClick={s.runCompare}>
        {s.status === "comparing" ? "Comparing…" : "Compare"}
      </button>
    </div>
  );
}
