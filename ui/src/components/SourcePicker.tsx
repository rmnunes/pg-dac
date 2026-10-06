import { useState } from "react";

import { pickDirectory, pickFile } from "../ipc/client";
import { useStore } from "../state/store";
import { defaultName, displaySpec, inferKind, isProfileRef, maskValue, profileRef } from "../state/profiles";

// One side of the compare (Source or Target). Supports a typed value (path /
// connection string), picking a saved profile (shown masked), and saving the
// current value as a new profile. Bound to store[side] (from | to).
export function SourcePicker({ side, label, hint }: { side: "from" | "to"; label: string; hint: string }) {
  const s = useStore();
  const value = s[side];
  const isProfile = isProfileRef(value);
  const [menuOpen, setMenuOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveName, setSaveName] = useState("");

  const set = (v: string) => s.set({ [side]: v } as never);
  const browseDir = async () => {
    const p = await pickDirectory();
    if (p) set(p);
  };
  const browseFile = async () => {
    const p = await pickFile([{ name: "pg-dac artifact", extensions: ["pgdac"] }]);
    if (p) set(p);
  };

  const beginSave = () => {
    setSaveName(defaultName(value));
    setSaving(true);
    setMenuOpen(false);
  };
  const commitSave = async () => {
    await s.addProfile({ name: saveName.trim() || defaultName(value), kind: inferKind(value), value });
    setSaving(false);
    // Switch this side to the just-saved profile so the value is now masked.
    const saved = useStore.getState().profiles.find((p) => p.value === value && p.kind === inferKind(value));
    if (saved) set(profileRef(saved.id));
  };

  return (
    <div className="source">
      <label>
        {label} <span className="role-hint">{hint}</span>
      </label>

      <div className="source-input">
        {isProfile ? (
          <div className="profile-chip" title="saved source">
            <span className="chip-name">{displaySpec(value, s.profiles)}</span>
            <button className="chip-x" title="clear" onClick={() => set("")}>
              ×
            </button>
          </div>
        ) : (
          <input
            value={value}
            onChange={(e) => set(e.target.value)}
            placeholder="postgres://…  |  ./project  |  app.pgdac"
          />
        )}

        <button className="saved-btn" title="saved sources" onClick={() => setMenuOpen((o) => !o)}>
          ▾
        </button>
        {!isProfile && (
          <>
            <button onClick={browseDir} title="pick a project directory">📁</button>
            <button onClick={browseFile} title="pick a .pgdac artifact">📦</button>
            {value.trim() !== "" && (
              <button onClick={beginSave} title="save this source">💾</button>
            )}
          </>
        )}
      </div>

      {menuOpen && (
        <div className="saved-menu" onMouseLeave={() => setMenuOpen(false)}>
          {s.profiles.length === 0 ? (
            <div className="muted saved-empty">No saved sources yet — type one and click 💾</div>
          ) : (
            s.profiles.map((p) => (
              <div className="saved-item" key={p.id}>
                <button
                  className="saved-pick"
                  onClick={() => {
                    set(profileRef(p.id));
                    setMenuOpen(false);
                  }}
                >
                  <span className={"kind-badge kind-" + p.kind}>{p.kind}</span>
                  <span className="saved-name">{p.name}</span>
                  <span className="saved-sub">{maskValue(p.kind, p.value)}</span>
                </button>
                <button className="saved-del" title="delete" onClick={() => s.removeProfile(p.id)}>
                  ×
                </button>
              </div>
            ))
          )}
        </div>
      )}

      {saving && (
        <div className="save-row">
          <input
            autoFocus
            value={saveName}
            onChange={(e) => setSaveName(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && commitSave()}
            placeholder="name (e.g. Dev)"
          />
          <button className="primary" onClick={commitSave}>Save</button>
          <button onClick={() => setSaving(false)}>Cancel</button>
        </div>
      )}
    </div>
  );
}
