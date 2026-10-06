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

// The editors share the application's dark surfaces. Token colours are chosen
// for ≥ 4.5:1 against the #1e1e22 background (vs-dark's defaults for comments
// and some SQL keywords fall below that).
const colors = {
  "editor.background": "#1e1e22",
  "editor.foreground": "#e6e6ea",
  "editorLineNumber.foreground": "#8c8c98",
  "editorLineNumber.activeForeground": "#e6e6ea",
  "editor.lineHighlightBackground": "#26262b",
  "editor.selectionBackground": "#2f3a4d",
  "editorGutter.background": "#1e1e22",
  "editorIndentGuide.background": "#3a3a42",
  "scrollbarSlider.background": "#3a3a4280",
  "scrollbarSlider.hoverBackground": "#4a4a54a0",
  "focusBorder": "#8ab8ff",
};

const rules: monaco.editor.ITokenThemeRule[] = [
  { token: "comment", foreground: "9cbf7a" },
  { token: "keyword", foreground: "7fb0ff" },
  { token: "operator", foreground: "d4d4d4" },
  { token: "string", foreground: "e8b08a" },
  { token: "number", foreground: "b5cea8" },
  { token: "type", foreground: "5fd3bd" },
  { token: "predefined", foreground: "5fd3bd" },
  { token: "identifier", foreground: "e6e6ea" },
  { token: "delimiter", foreground: "d4d4d4" },
];

monaco.editor.defineTheme("pgdac-dark", { base: "vs-dark", inherit: true, rules, colors });

// SSDT-style diff colouring. Source is on the LEFT (Monaco's "original"), Target
// on the RIGHT ("modified"); Update pushes Source → Target. A line only in the
// Source will be ADDED to the Target and a line only in the Target will be
// REMOVED — so Monaco's palette is swapped: original-only (Source, left) →
// green (added), modified-only (Target, right) → red (removed). The +/- gutter
// glyphs are hidden (renderIndicators:false) so nothing contradicts the colours.
monaco.editor.defineTheme("pgdac-diff", {
  base: "vs-dark",
  inherit: true,
  rules,
  colors: {
    ...colors,
    // "removed" = Source-only (left) → render as additions (green)
    "diffEditor.removedLineBackground": "#1f3a2880",
    "diffEditor.removedTextBackground": "#3fae6a66",
    "diffEditorGutter.removedLineBackground": "#1f3a28",
    "diffEditorOverview.removedForeground": "#4dbf79",
    // "inserted" = Target-only (right) → render as removals (red)
    "diffEditor.insertedLineBackground": "#3a202080",
    "diffEditor.insertedTextBackground": "#d35c5c66",
    "diffEditorGutter.insertedLineBackground": "#3a2020",
    "diffEditorOverview.insertedForeground": "#ef9393",
  },
});
