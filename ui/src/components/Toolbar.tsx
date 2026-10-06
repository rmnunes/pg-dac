import { FileCode2, Play, Settings2, Upload } from "lucide-react";

import { changedOptionCount, useStore } from "../state/store";
import { specIsDb, specIsProject } from "../state/profiles";

export function Toolbar({ onOptions, onScript, onPublish }: { onOptions: () => void; onScript: () => void; onPublish: () => void }) {
  const s = useStore();
  const hasChanges = !!s.plan && s.plan.changes.length > 0;
  // A Target is published to when it is a live database (the migration runs
  // there) or a project directory (its files are updated from the Source). An
  // artifact cannot be a Target.
  const targetIsDb = specIsDb(s.from, s.profiles);
  const targetIsProject = specIsProject(s.from, s.profiles);
  const optionCount = changedOptionCount(s);
  const comparing = s.status === "comparing";
  const scripting = s.status === "scripting";

  return (
    <div className="toolbar" role="toolbar" aria-label="Actions">
      <button type="button" className="primary" disabled={!s.from || !s.to || comparing} title="Compare (Ctrl+Enter)" onClick={() => void s.runCompare()}>
        <Play size={14} aria-hidden="true" />
        {comparing ? "Comparing…" : "Compare"}
      </button>
      <span className="sep" aria-hidden="true" />
      <button type="button" disabled={!hasChanges || scripting} title="Render the migration as one SQL script" onClick={onScript}>
        <FileCode2 size={14} aria-hidden="true" />
        {scripting ? "Generating…" : "Generate Script"}
      </button>
      <button
        type="button"
        disabled={!hasChanges || !(targetIsDb || targetIsProject)}
        title={
          targetIsDb
            ? "Apply the changes to the Target database"
            : targetIsProject
              ? "Rewrite the Target project's files so they describe the Source (SSDT's Update project from database)"
              : "Publish updates the Target — set the Target to a database connection or a project directory"
        }
        onClick={onPublish}
      >
        <Upload size={14} aria-hidden="true" />
        {targetIsProject ? "Update Target project…" : "Publish to Target…"}
      </button>
      <span className="sep" aria-hidden="true" />
      <button type="button" onClick={onOptions} aria-label={optionCount ? `Options, ${optionCount} changed` : "Options"} title="Comparison and deployment options">
        <Settings2 size={14} aria-hidden="true" />
        Options
        {optionCount > 0 && (
          <span className="count-pill" aria-hidden="true">
            {optionCount}
          </span>
        )}
      </button>
    </div>
  );
}
