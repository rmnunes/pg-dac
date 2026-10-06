// Bundle Monaco from the local package (no CDN) so it loads under the app's
// Content-Security-Policy, and wire the editor web worker via Vite. Imported for
// its side effects before the app renders.
import { loader } from "@monaco-editor/react";
import * as monaco from "monaco-editor";
import EditorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";

(globalThis as unknown as { MonacoEnvironment: unknown }).MonacoEnvironment = {
  getWorker() {
    return new EditorWorker();
  },
};

loader.config({ monaco });

// SSDT-style diff coloring. Source is on the LEFT (Monaco's "original"), Target on
// the RIGHT ("modified"); Update pushes Source → Target. A line only in the Source
// will be ADDED to the Target, and a line only in the Target will be REMOVED — so
// we swap Monaco's palette: original-only (Source, left) → green (added), and
// modified-only (Target, right) → red (removed). The +/- gutter glyphs are hidden
// (renderIndicators:false in the DiffEditor) so nothing contradicts the colors.
monaco.editor.defineTheme("pgdac-diff", {
  base: "vs",
  inherit: true,
  rules: [],
  colors: {
    // "removed" = Source-only (left) → render as additions (green)
    "diffEditor.removedLineBackground": "#e6ffed",
    "diffEditor.removedTextBackground": "#abf2bc",
    "diffEditorGutter.removedLineBackground": "#cdffd8",
    "diffEditorOverview.removedForeground": "#3fae6a",
    // "inserted" = Target-only (right) → render as removals (red)
    "diffEditor.insertedLineBackground": "#ffeef0",
    "diffEditor.insertedTextBackground": "#fdb8c0",
    "diffEditorGutter.insertedLineBackground": "#ffdce0",
    "diffEditorOverview.insertedForeground": "#d35c5c",
  },
});
