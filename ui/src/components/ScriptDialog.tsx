import { useEffect, useState } from "react";
import { Editor } from "@monaco-editor/react";
import { Check, Copy } from "lucide-react";

import { READ_ONLY_OPTIONS, THEME } from "../monaco-setup";
import { Dialog } from "./Dialog";

export function ScriptDialog({ sql, onClose }: { sql: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(false), 1600);
    return () => clearTimeout(t);
  }, [copied]);

  const copy = async () => {
    try {
      await navigator.clipboard?.writeText(sql);
      setCopied(true);
    } catch {
      /* clipboard unavailable: the text stays selectable in the editor */
    }
  };

  return (
    <Dialog
      title="Migration script"
      onClose={onClose}
      className="tall"
      width={960}
      headerExtra={
        <button type="button" onClick={() => void copy()} aria-live="polite">
          {copied ? <Check size={14} aria-hidden="true" /> : <Copy size={14} aria-hidden="true" />}
          {copied ? "Copied" : "Copy"}
        </button>
      }
    >
      <div className="editor">
        <Editor language="sql" theme={THEME} value={sql} options={READ_ONLY_OPTIONS} />
      </div>
      <div className="dialog-actions">
        <span className="hint">Read-only. Copy it into your deployment pipeline or run it with psql.</span>
        <button type="button" className="primary" onClick={onClose}>
          Close
        </button>
      </div>
    </Dialog>
  );
}
