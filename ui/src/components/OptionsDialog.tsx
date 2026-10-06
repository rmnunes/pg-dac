import { useId, useState, type ReactNode } from "react";

import { OPTION_DEFAULTS, optionsStale, useStore } from "../state/store";
import { Dialog } from "./Dialog";

export type OptionsTab = "compare" | "deploy";

function Field({ id, label, desc, danger, children }: { id: string; label: string; desc: string; danger?: boolean; children: ReactNode }) {
  return (
    <div className={"field" + (danger ? " danger" : "")}>
      <label htmlFor={id}>{label}</label>
      <div className="control">
        {children}
        <div className="field-desc" id={id + "-desc"}>
          {desc}
        </div>
      </div>
    </div>
  );
}

function Check({
  id,
  label,
  desc,
  checked,
  onChange,
  danger,
}: {
  id: string;
  label: string;
  desc: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  danger?: boolean;
}) {
  return (
    <div className={"field check" + (danger ? " danger" : "")}>
      <input id={id} type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} aria-describedby={id + "-desc"} />
      <div className="control">
        <label htmlFor={id}>{label}</label>
        <div className="field-desc" id={id + "-desc"}>
          {desc}
        </div>
      </div>
    </div>
  );
}

// Comparison semantics in one tab, deployment switches in the other (SSDT keeps
// them apart too: Schema Compare Options vs. the publish profile). A "" value on
// a semantics option defers to the Source project's pgdac.yaml compare: block.
export function OptionsDialog({ initialTab, onClose }: { initialTab: OptionsTab; onClose: () => void }) {
  const s = useStore();
  const [tab, setTab] = useState<OptionsTab>(initialTab);
  const uid = useId();
  const id = (k: string) => `${uid}-${k}`;
  const stale = optionsStale(s);

  const tabs: { key: OptionsTab; label: string }[] = [
    { key: "compare", label: "Comparison" },
    { key: "deploy", label: "Deployment" },
  ];
  const onTabKey = (e: React.KeyboardEvent) => {
    const i = tabs.findIndex((t) => t.key === tab);
    if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
      e.preventDefault();
      const n = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length].key;
      setTab(n);
      document.getElementById(id("tab-" + n))?.focus();
    }
  };

  return (
    <Dialog title="Options" onClose={onClose} width={720}>
      <div className="opt-tabs" role="tablist" aria-label="Option groups" onKeyDown={onTabKey}>
        {tabs.map((t) => (
          <button
            key={t.key}
            type="button"
            id={id("tab-" + t.key)}
            role="tab"
            className="tab"
            aria-selected={tab === t.key}
            aria-controls={id("panel-" + t.key)}
            tabIndex={tab === t.key ? 0 : -1}
            onClick={() => setTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === "compare" && (
        <div className="fields" role="tabpanel" id={id("panel-compare")} aria-labelledby={id("tab-compare")}>
          <Field id={id("schemas")} label="Schemas" desc="Compare only these schemas, comma-separated. Empty: every schema the Source defines.">
            <input id={id("schemas")} value={s.schemas} onChange={(e) => s.set({ schemas: e.target.value })} placeholder="app, audit" aria-describedby={id("schemas-desc")} />
          </Field>
          <Field id={id("major")} label="Target PostgreSQL version" desc="Major version to generate SQL for. Empty: detected from the Target.">
            <input id={id("major")} inputMode="numeric" value={s.targetMajor} onChange={(e) => s.set({ targetMajor: e.target.value.replace(/[^0-9]/g, "") })} placeholder="detected" style={{ width: 120 }} aria-describedby={id("major-desc")} />
          </Field>
          <Field
            id={id("priv")}
            label="Privileges"
            desc="GRANT and REVOKE are part of the model. Ignore leaves the Target's privileges alone; a recreated object still gets its privileges re-granted."
          >
            <select id={id("priv")} value={s.privileges} onChange={(e) => s.set({ privileges: e.target.value as typeof s.privileges })} aria-describedby={id("priv-desc")}>
              <option value="">Project default (compare)</option>
              <option value="compare">Compare</option>
              <option value="ignore">Ignore</option>
            </select>
          </Field>
          <Field
            id={id("colorder")}
            label="Column order"
            desc="PostgreSQL cannot reorder columns in place. Warn reports a table whose declared order differs; Rebuild copies the table into one with the declared order (SSDT behaviour, heavy); Ignore stays silent."
          >
            <select id={id("colorder")} value={s.columnOrder} onChange={(e) => s.set({ columnOrder: e.target.value as typeof s.columnOrder })} aria-describedby={id("colorder-desc")}>
              <option value="">Project default (warn)</option>
              <option value="ignore">Ignore</option>
              <option value="warn">Warn</option>
              <option value="rebuild">Rebuild the table</option>
            </select>
          </Field>
          <Field id={id("ws")} label="Routine whitespace" desc="Routine bodies are stored verbatim. Ignore treats whitespace-only differences as equal.">
            <select id={id("ws")} value={s.whitespace} onChange={(e) => s.set({ whitespace: e.target.value as typeof s.whitespace })} aria-describedby={id("ws-desc")}>
              <option value="">Project default (ignore)</option>
              <option value="ignore">Ignore</option>
              <option value="compare">Compare</option>
            </select>
          </Field>
          <Field id={id("comments")} label="Routine comments" desc="Whether a comment-only change inside a routine body counts as a change.">
            <select id={id("comments")} value={s.comments} onChange={(e) => s.set({ comments: e.target.value as typeof s.comments })} aria-describedby={id("comments-desc")}>
              <option value="">Project default (compare)</option>
              <option value="compare">Compare</option>
              <option value="ignore">Ignore</option>
            </select>
          </Field>
          <Field
            id={id("exclude")}
            label="Exclude object types"
            desc="Left out of the comparison, comma-separated: table, view, matview, sequence, function, enum, domain, compositeType, collation, schema, extension, index, constraint, trigger, policy, privilege, comment."
          >
            <input id={id("exclude")} value={s.excludeTypes} onChange={(e) => s.set({ excludeTypes: e.target.value })} placeholder="trigger, comment" aria-describedby={id("exclude-desc")} />
          </Field>
          <Field id={id("key")} label="Require signer key" desc="Refuse a .pgdac Source unless it is signed by this key fingerprint.">
            <input id={id("key")} className="mono" value={s.requireKey} onChange={(e) => s.set({ requireKey: e.target.value })} placeholder="SHA256:…" aria-describedby={id("key-desc")} />
          </Field>
        </div>
      )}

      {tab === "deploy" && (
        <div className="fields" role="tabpanel" id={id("panel-deploy")} aria-labelledby={id("tab-deploy")}>
          <Check
            id={id("online")}
            label="Online (lock-safe)"
            desc="Build indexes CONCURRENTLY and add lock timeouts with retries so the migration does not queue behind long-running queries. Turn off to run everything in one transaction."
            checked={s.online}
            onChange={(v) => s.set({ online: v })}
          />
          <Check
            id={id("single")}
            label="Single transaction"
            desc="Consecutive transactional statements run in one transaction and roll back together on failure. CONCURRENTLY statements still run on their own between transactions."
            checked={s.singleTransaction}
            onChange={(v) => s.set({ singleTransaction: v })}
          />
          <Check
            id={id("drop")}
            label="Drop objects not in the Source"
            desc="Target objects the Source does not define are dropped. Off: they are kept and listed in the notes."
            checked={s.dropObjects}
            onChange={(v) => s.set({ dropObjects: v })}
            danger
          />
          <Check
            id={id("drift")}
            label="Allow drift"
            desc="Publish even if the Target changed outside pg-dac since its last recorded baseline."
            checked={s.allowDrift}
            onChange={(v) => s.set({ allowDrift: v })}
            danger
          />
          <Check
            id={id("skip")}
            label="Skip pre/post-deploy scripts"
            desc="Do not run the Source project's pre- and post-deploy scripts around the migration."
            checked={s.skipDeployScripts}
            onChange={(v) => s.set({ skipDeployScripts: v })}
          />
          <Field id={id("vars")} label="Deploy variables" desc="Values for $(name) placeholders in pre/post-deploy scripts, one name=value per line. Overrides the project's variables.">
            <textarea id={id("vars")} rows={3} value={s.variables} onChange={(e) => s.set({ variables: e.target.value })} placeholder={"deploy_environment=dev"} aria-describedby={id("vars-desc")} />
          </Field>
        </div>
      )}

      <div className="dialog-actions">
        <span className="hint" role="status">
          {stale ? "Options changed since the last comparison — compare again to apply them." : "Changes take effect on the next Compare."}
        </span>
        <button type="button" onClick={() => s.set({ ...OPTION_DEFAULTS })}>
          Reset to defaults
        </button>
        <button type="button" className="primary" onClick={onClose}>
          Done
        </button>
      </div>
    </Dialog>
  );
}
