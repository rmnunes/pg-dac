import { Editor } from "@monaco-editor/react";

export function ScriptModal({ sql, onClose }: { sql: string; onClose: () => void }) {
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal script" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h2>Migration script</h2>
          <button onClick={() => navigator.clipboard?.writeText(sql)}>Copy</button>
        </div>
        <div className="editor">
          <Editor language="sql" value={sql} options={{ readOnly: true, minimap: { enabled: false }, fontSize: 12 }} />
        </div>
        <div className="modal-actions">
          <button onClick={onClose}>Close</button>
        </div>
      </div>
    </div>
  );
}
