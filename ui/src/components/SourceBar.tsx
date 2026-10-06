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
          <label>Require signer key <input value={s.requireKey} onChange={(e) => s.set({ requireKey: e.target.value })} placeholder="fingerprint of a .pgdac signer" /></label>
          <label><input type="checkbox" checked={s.skipDeployScripts} onChange={(e) => s.set({ skipDeployScripts: e.target.checked })} /> Skip pre/post-deploy scripts</label>
          <label className="danger"><input type="checkbox" checked={s.allowDrift} onChange={(e) => s.set({ allowDrift: e.target.checked })} /> Allow drift (publish even if the Target changed outside pg-dac)</label>
          {/* Comparison semantics; "" defers to the Source project's pgdac.yaml compare: block. */}
          <label title="GRANT/REVOKE are part of the model. Ignoring them still re-grants a recreated object's privileges.">
            Privileges{" "}
            <select value={s.privileges} onChange={(e) => s.set({ privileges: e.target.value as typeof s.privileges })}>
              <option value="">project default (compare)</option>
              <option value="compare">compare</option>
              <option value="ignore">ignore</option>
            </select>
          </label>
          <label title="PostgreSQL cannot reorder columns in place. 'rebuild' copies the table into one with the declared order (SSDT-style; heavy).">
            Column order{" "}
            <select value={s.columnOrder} onChange={(e) => s.set({ columnOrder: e.target.value as typeof s.columnOrder })}>
              <option value="">project default (warn)</option>
              <option value="ignore">ignore</option>
              <option value="warn">warn</option>
              <option value="rebuild">rebuild table</option>
            </select>
          </label>
          <label title="Routine bodies are stored verbatim; by default whitespace differences are not changes.">
            Routine whitespace{" "}
            <select value={s.whitespace} onChange={(e) => s.set({ whitespace: e.target.value as typeof s.whitespace })}>
              <option value="">project default (ignore)</option>
              <option value="ignore">ignore</option>
              <option value="compare">compare</option>
            </select>
          </label>
          <label title="Whether a comment-only change inside a routine body is a change.">
            Routine comments{" "}
            <select value={s.comments} onChange={(e) => s.set({ comments: e.target.value as typeof s.comments })}>
              <option value="">project default (compare)</option>
              <option value="compare">compare</option>
              <option value="ignore">ignore</option>
            </select>
          </label>
          <label title="Object types left out of the comparison (SSDT ExcludeObjectTypes): table, view, matview, sequence, function, enum, domain, compositeType, collation, schema, extension, index, constraint, trigger, policy, privilege, comment">
            Exclude object types{" "}
            <input value={s.excludeTypes} onChange={(e) => s.set({ excludeTypes: e.target.value })} placeholder="trigger, comment" />
          </label>
          <label title="Values for $(name) placeholders in the project's pre/post-deploy scripts, one name=value per line (overrides pgdac.yaml variables:)">
            Deploy variables{" "}
            <textarea value={s.variables} onChange={(e) => s.set({ variables: e.target.value })} placeholder={"deploy_environment=dev"} rows={2} />
          </label>
        </div>
      </details>

      <button className="primary compare" disabled={!s.from || !s.to || s.status === "comparing"} onClick={s.runCompare}>
        {s.status === "comparing" ? "Comparing…" : "Compare"}
      </button>
    </div>
  );
}
