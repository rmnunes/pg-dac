import { useEffect, useId, useRef, useState } from "react";
import { ChevronDown, Database, FolderOpen, Package, Save, Trash2, X } from "lucide-react";

import { pickDirectory, pickFile } from "../ipc/client";
import { useStore, type Side } from "../state/store";
import { defaultName, displaySpec, inferKind, isProfileRef, maskValue, profileOf, profileRef } from "../state/profiles";
import type { ProfileKind } from "../ipc/types";

function KindIcon({ kind, size = 15 }: { kind: ProfileKind | undefined; size?: number }) {
  const cls = "kind-icon" + (kind ? " kind-" + kind : "");
  if (kind === "db") return <Database className={cls} size={size} aria-hidden="true" />;
  if (kind === "project") return <FolderOpen className={cls} size={size} aria-hidden="true" />;
  if (kind === "artifact") return <Package className={cls} size={size} aria-hidden="true" />;
  return <Database className={cls} size={size} aria-hidden="true" />;
}

// One side of the compare (Source or Target), SSDT-style: a dropdown whose menu
// holds the saved sources plus the ways to provide a new one (browse for a
// project folder, open a .pgdac artifact, type a connection string or path).
// A typed connection string is shown masked ("database @ host"); it can be kept
// as a saved source. Bound to store[side] (from | to).
export function SourcePicker({ side, label, hint }: { side: Side; label: string; hint: string }) {
  const value = useStore((s) => s[side]);
  const profiles = useStore((s) => s.profiles);
  const { setSource, addProfile, removeProfile } = useStore.getState();
  const profile = profileOf(value, profiles);
  const isProfile = isProfileRef(value);
  const labelId = useId();
  const savedId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const [typing, setTyping] = useState(false);
  const [draft, setDraft] = useState("");
  const [saving, setSaving] = useState(false);
  const [saveName, setSaveName] = useState("");

  const set = (v: string) => setSource(side, v);
  // Leaving the typing or saving row hands focus back to the picker button.
  const refocus = useRef(false);
  useEffect(() => {
    if (!typing && !saving && refocus.current) {
      refocus.current = false;
      buttonRef.current?.focus();
    }
  }, [typing, saving]);
  const endTyping = () => {
    refocus.current = true;
    setTyping(false);
  };
  const endSaving = () => {
    refocus.current = true;
    setSaving(false);
  };

  // Close the menu on an outside click.
  useEffect(() => {
    if (!menuOpen) return;
    const onDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setMenuOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [menuOpen]);

  // Focus the first entry when the menu opens.
  useEffect(() => {
    if (menuOpen) menuRef.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
  }, [menuOpen]);

  const closeMenu = (refocus = true) => {
    setMenuOpen(false);
    if (refocus) buttonRef.current?.focus();
  };

  const onMenuKey = (e: React.KeyboardEvent) => {
    const items = [...(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
    const i = items.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      e.preventDefault();
      closeMenu();
    } else if (e.key === "Tab") {
      closeMenu(false);
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      items[(i + 1) % items.length]?.focus();
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      items[(i - 1 + items.length) % items.length]?.focus();
    } else if (e.key === "Home") {
      e.preventDefault();
      items[0]?.focus();
    } else if (e.key === "End") {
      e.preventDefault();
      items[items.length - 1]?.focus();
    }
  };

  const browseDir = async () => {
    closeMenu(false);
    const p = await pickDirectory();
    if (p) set(p);
    buttonRef.current?.focus();
  };
  const browseFile = async () => {
    closeMenu(false);
    const p = await pickFile([{ name: "pg-dac artifact", extensions: ["pgdac"] }]);
    if (p) set(p);
    buttonRef.current?.focus();
  };
  const startTyping = () => {
    closeMenu(false);
    setDraft(isProfile ? "" : value);
    setTyping(true);
  };
  const commitTyping = () => {
    set(draft.trim());
    endTyping();
  };

  const beginSave = () => {
    setSaveName(defaultName(value));
    setSaving(true);
  };
  const commitSave = async () => {
    const saved = await addProfile({ name: saveName.trim() || defaultName(value), kind: inferKind(value), value });
    endSaving();
    // Switch this side to the just-saved profile so the value is now masked.
    if (saved) set(profileRef(saved.id));
  };

  const kind: ProfileKind | undefined = profile ? profile.kind : value ? inferKind(value) : undefined;

  return (
    <div className="picker" ref={rootRef}>
      <label id={labelId}>
        <b>{label}</b>
        <span>{hint}</span>
      </label>

      <div className="picker-row">
        {typing ? (
          <>
            <input
              autoFocus
              aria-labelledby={labelId}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="postgres://user@host/db  ·  C:\path\to\project  ·  app.pgdac"
              onKeyDown={(e) => {
                if (e.key === "Enter") commitTyping();
                if (e.key === "Escape") endTyping();
              }}
            />
            <button type="button" className="primary" onClick={commitTyping}>
              Use
            </button>
            <button type="button" onClick={endTyping}>
              Cancel
            </button>
          </>
        ) : (
          <>
            <button
              ref={buttonRef}
              type="button"
              className={"picker-btn" + (value ? " filled" : "")}
              aria-labelledby={labelId}
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              onClick={() => setMenuOpen((o) => !o)}
            >
              <KindIcon kind={kind} />
              <span className="picker-text">
                {!value ? (
                  <span className="muted">Choose a source…</span>
                ) : profile ? (
                  <>
                    {profile.name}
                    <span className="muted">{maskValue(profile.kind, profile.value)}</span>
                  </>
                ) : (
                  displaySpec(value, profiles)
                )}
              </span>
              <ChevronDown className="chev" size={16} aria-hidden="true" />
            </button>
            {value && !isProfile && (
              <button type="button" className="icon-btn" title="Keep as a saved source" aria-label={`Keep the ${label} as a saved source`} onClick={beginSave}>
                <Save size={15} aria-hidden="true" />
              </button>
            )}
            {value && (
              <button type="button" className="icon-btn" title="Clear" aria-label={`Clear the ${label}`} onClick={() => set("")}>
                <X size={15} aria-hidden="true" />
              </button>
            )}
          </>
        )}
      </div>

      {menuOpen && (
        <div className="picker-menu" role="menu" aria-label={`${label} sources`} ref={menuRef} onKeyDown={onMenuKey}>
          <div role="group" aria-labelledby={savedId}>
            <div className="menu-heading" id={savedId} role="presentation">
              Saved sources
            </div>
            {profiles.length === 0 ? (
              <div className="menu-empty" role="menuitem" aria-disabled="true" tabIndex={-1}>
                None yet. Choose a source below, then use the save button next to it.
              </div>
            ) : (
              profiles.map((p) => (
                <div className="menu-item" role="none" key={p.id}>
                  <button
                    type="button"
                    role="menuitem"
                    tabIndex={-1}
                    onClick={() => {
                      set(profileRef(p.id));
                      closeMenu();
                    }}
                  >
                    <KindIcon kind={p.kind} />
                    <span className="item-name">{p.name}</span>
                    <span className="item-sub">{maskValue(p.kind, p.value)}</span>
                  </button>
                  <button type="button" role="menuitem" tabIndex={-1} className="icon-btn" aria-label={`Delete saved source ${p.name}`} title="Delete" onClick={() => void removeProfile(p.id)}>
                    <Trash2 size={14} aria-hidden="true" />
                  </button>
                </div>
              ))
            )}
          </div>
          <div className="menu-sep" role="separator" />
          <div role="group" aria-label="Other sources">
            <div className="menu-item" role="none">
              <button type="button" role="menuitem" tabIndex={-1} onClick={browseDir}>
                <FolderOpen className="kind-icon kind-project" size={15} aria-hidden="true" />
                Browse for a project folder…
              </button>
            </div>
            <div className="menu-item" role="none">
              <button type="button" role="menuitem" tabIndex={-1} onClick={browseFile}>
                <Package className="kind-icon kind-artifact" size={15} aria-hidden="true" />
                Open a .pgdac artifact…
              </button>
            </div>
            <div className="menu-item" role="none">
              <button type="button" role="menuitem" tabIndex={-1} onClick={startTyping}>
                <Database className="kind-icon kind-db" size={15} aria-hidden="true" />
                Type a connection string or path…
              </button>
            </div>
          </div>
        </div>
      )}

      {saving && (
        <div className="picker-row">
          <input
            autoFocus
            aria-label={`Name for the saved ${label}`}
            value={saveName}
            onChange={(e) => setSaveName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void commitSave();
              if (e.key === "Escape") endSaving();
            }}
            placeholder="Name, e.g. Dev"
          />
          <button type="button" className="primary" onClick={() => void commitSave()}>
            Save
          </button>
          <button type="button" onClick={endSaving}>
            Cancel
          </button>
        </div>
      )}
    </div>
  );
}
