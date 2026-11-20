import { useId, useState, type ReactNode } from "react";

import { optionsStale, useStore } from "../state/store";
import { Dialog } from "./Dialog";
import { TabList, panelId, tabId, type TabSpec } from "./TabList";

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
  // The dialog edits every option, so it reads the whole options slice.
  const s = useStore();
  const { setOptions, resetOptions } = s;
  const [tab, setTab] = useState<OptionsTab>(initialTab);
  const uid = useId();
  const id = (k: string) => `${uid}-${k}`;
  const stale = optionsStale(s);

  const tabs: TabSpec<OptionsTab>[] = [
    { key: "compare", label: "Comparison" },
    { key: "deploy", label: "Deployment" },
  ];

  return (
    <Dialog title="Options" onClose={onClose} width={720}>
      <TabList base={uid} tabs={tabs} active={tab} onSelect={setTab} label="Option groups" className="opt-tabs" />

      {tab === "compare" && (
        <div className="fields" role="tabpanel" id={panelId(uid, "compare")} aria-labelledby={tabId(uid, "compare")}>
          <Field id={id("schemas")} label="Schemas" desc="Compare only these schemas, comma-separated. Empty: every schema the Source defines.">
            <input id={id("schemas")} value={s.schemas} onChange={(e) => setOptions({ schemas: e.target.value })} placeholder="app, audit" aria-describedby={id("schemas-desc")} />
          </Field>
          <Field id={id("major")} label="Target PostgreSQL version" desc="Major version to generate SQL for. Empty: detected from the Target.">
            <input id={id("major")} inputMode="numeric" value={s.targetMajor} onChange={(e) => setOptions({ targetMajor: e.target.value.replace(/[^0-9]/g, "") })} placeholder="detected" style={{ width: 120 }} aria-describedby={id("major-desc")} />
          </Field>
          <Field
            id={id("priv")}
            label="Privileges"
            desc="GRANT and REVOKE are part of the model. Ignore leaves the Target's privileges alone; a recreated object still gets its privileges re-granted."
          >
            <select id={id("priv")} value={s.privileges} onChange={(e) => setOptions({ privileges: e.target.value as typeof s.privileges })} aria-describedby={id("priv-desc")}>
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
            <select id={id("colorder")} value={s.columnOrder} onChange={(e) => setOptions({ columnOrder: e.target.value as typeof s.columnOrder })} aria-describedby={id("colorder-desc")}>
              <option value="">Project default (warn)</option>
              <option value="ignore">Ignore</option>
              <option value="warn">Warn</option>
              <option value="rebuild">Rebuild the table</option>
            </select>
          </Field>
          <Field id={id("ws")} label="Routine whitespace" desc="Routine bodies are stored verbatim. Ignore treats whitespace-only differences as equal.">
            <select id={id("ws")} value={s.whitespace} onChange={(e) => setOptions({ whitespace: e.target.value as typeof s.whitespace })} aria-describedby={id("ws-desc")}>
              <option value="">Project default (ignore)</option>
              <option value="ignore">Ignore</option>
              <option value="compare">Compare</option>
            </select>
          </Field>
          <Field id={id("comments")} label="Routine comments" desc="Whether a comment-only change inside a routine body counts as a change.">
            <select id={id("comments")} value={s.comments} onChange={(e) => setOptions({ comments: e.target.value as typeof s.comments })} aria-describedby={id("comments-desc")}>
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
            <input id={id("exclude")} value={s.excludeTypes} onChange={(e) => setOptions({ excludeTypes: e.target.value })} placeholder="trigger, comment" aria-describedby={id("exclude-desc")} />
          </Field>
          <Field id={id("key")} label="Require signer key" desc="Refuse a .pgdac Source unless it is signed by this key fingerprint.">
            <input id={id("key")} className="mono" value={s.requireKey} onChange={(e) => setOptions({ requireKey: e.target.value })} placeholder="SHA256:…" aria-describedby={id("key-desc")} />
          </Field>
        </div>
      )}

      {tab === "deploy" && (
        <div className="fields" role="tabpanel" id={panelId(uid, "deploy")} aria-labelledby={tabId(uid, "deploy")}>
          <Check
            id={id("online")}
            label="Online (lock-safe)"
            desc="Build indexes CONCURRENTLY and add lock timeouts with retries so the migration does not queue behind long-running queries. Turn off to run everything in one transaction."
            checked={s.online}
            onChange={(v) => setOptions({ online: v })}
          />
          <Check
            id={id("single")}
            label="Single transaction"
            desc="Consecutive transactional statements run in one transaction and roll back together on failure. CONCURRENTLY statements still run on their own between transactions."
            checked={s.singleTransaction}
            onChange={(v) => setOptions({ singleTransaction: v })}
          />
          <Check
            id={id("drop")}
            label="Drop objects not in the Source"
            desc="Target objects the Source does not define are dropped. Off: they are kept and listed in the notes."
            checked={s.dropObjects}
            onChange={(v) => setOptions({ dropObjects: v })}
            danger
          />
          <Check
            id={id("drift")}
            label="Allow drift"
            desc="Publish even if the Target changed outside pg-dac since its last recorded baseline."
            checked={s.allowDrift}
            onChange={(v) => setOptions({ allowDrift: v })}
            danger
          />
          <Check
            id={id("skip")}
            label="Skip pre/post-deploy scripts"
            desc="Do not run the Source project's pre- and post-deploy scripts around the migration."
            checked={s.skipDeployScripts}
            onChange={(v) => setOptions({ skipDeployScripts: v })}
          />
          <Field id={id("vars")} label="Deploy variables" desc="Values for $(name) placeholders in pre/post-deploy scripts, one name=value per line. Overrides the project's variables.">
            <textarea id={id("vars")} rows={3} value={s.variables} onChange={(e) => setOptions({ variables: e.target.value })} placeholder={"deploy_environment=dev"} aria-describedby={id("vars-desc")} />
          </Field>
        </div>
      )}

      <div className="dialog-actions">
        <span className="hint" role="status">
          {stale ? "Options changed since the last comparison — compare again to apply them." : "Changes take effect on the next Compare."}
        </span>
        <button type="button" onClick={resetOptions}>
          Reset to defaults
        </button>
        <button type="button" className="primary" onClick={onClose}>
          Done
        </button>
      </div>
    </Dialog>
  );
}
