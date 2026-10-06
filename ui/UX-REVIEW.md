# pg-dac GUI — UX evaluation (2026-10-07)

Scope: the Tauri/React Schema Compare GUI in `ui/`, evaluated on the real frontend running in
a browser against the in-browser mock daemon (`src/ipc/mock.ts`, replaying a real plan with
5 changes, DATA LOSS, 3 engine warnings). Methods: Nielsen heuristic evaluation with severity
ratings, an automated accessibility audit (axe-core 4.10), a keyboard-only pass, a layout
measurement at two window sizes, and a cognitive walkthrough of the primary task (compare a
project to a database, review, publish).

Severity scale (Nielsen): 0 none · 1 cosmetic · 2 minor · 3 major · 4 catastrophic (blocks a task).

## 0. Outcome (same day)

Option A ("SSDT faithful", section 6) was chosen and implemented together with the foundation
layer. Re-audit on the mocked frontend after the redesign:

| Check | Before | After |
|---|---|---|
| axe-core violations | 1 critical (`label`), 1 serious (`color-contrast`), 3 moderate (landmarks, heading) | **0** |
| Workspace height at 1024×680 (notes open) | 151 px (22 %) | 393 px (58 %) |
| Escape closes dialogs / focus returns to the opener | no / no | yes / yes |
| Change list by keyboard | mouse only | arrows, Home/End, Space, Enter, Left/Right |
| Smallest text | 10 px | 12 px |
| Editor theme | light `vs` in a dark shell | dark, contrast-checked tokens |
| Hazard codes | raw (`ACQUIRES_ACCESS_EXCLUSIVE_LOCK`) | title + explanation + affected objects (`src/vocab.ts`) |
| Options | 12 controls in one wrapping row | dialog, Comparison / Deployment tabs, labelled and described |
| Warnings | permanent 106 px banner | collapsible, dismissible notes panel with a status-bar toggle |

Sections 1–6 below are the evaluation as written before the redesign.

## 1. Summary

The GUI is functionally complete (every engine capability is reachable) but it presents three
distinct jobs (set up the comparison, review the changes, deploy) on one screen at once, so the
part that matters — the change list and the diff — gets less than half of the window, and on a
small window almost none. The option panel is a flat list of 12 controls that mixes comparison
semantics with deployment switches. Warnings are permanent banners written in engine/CLI
vocabulary. Accessibility fails at the basic level: no form control has an accessible name, the
dialogs are not dialogs to assistive technology, Escape does nothing, the change list cannot be
navigated with the keyboard, and the primary buttons and the "danger" labels fail the contrast
minimum. The code editors use a light theme inside a dark application.

None of this is hard to fix; the architecture (Zustand store, IPC types, Monaco, virtualized
list) is sound and stays. The question is how far to restructure the screen, and section 6
offers three options.

## 2. Measurements

| What | Value |
|---|---|
| Default window / minimum (tauri.conf.json) | 1400×900 / 900×600 |
| Header stack at 1366×820, Options expanded (title 34 + sources 265 + direction 29 + warnings 106) | 434 px |
| Workspace (change list + diff) at 1366×820 | 345 px — 42 % of the window |
| Header stack at 1024×680 (near the minimum) | 488 px; workspace 151 px — 22 % |
| Warnings banner (3 engine warnings + engine note), always visible, not dismissible | 106 px |
| Root font size / controls / object type / badges and role hints | 13 / 12 / 11 / 10 px |
| Tabbable controls on the results screen | 30 (none of them is a change row) |

## 3. Accessibility audit (axe-core 4.10, results screen with Options expanded)

| Rule | Impact | Nodes | Detail |
|---|---|---|---|
| `label` — form elements must have labels | critical | 11 checkboxes | every checkbox is announced as "on" (Options switches, "hazards only", each change row, the dialog acknowledgements) |
| `color-contrast` | serious | 6 + editor | `.danger` labels `#d35c5c` on `#2d2d33` = 3.55:1; primary buttons white on `#4f8cf0` = 3.31:1; "1 drop" count 3.55:1; selected row's object type 4.11:1; Monaco `vs-light` tokens (`#778899` on white = 3.63:1) |
| `landmark-one-main`, `region`, `page-has-heading-one` | moderate | page | no landmarks, no heading hierarchy |
| (manual) accessible names | — | 4 selects, 6 text inputs, 6 icon buttons | selects are named by their current value ("project default (warn)"), inputs only by placeholder, icon buttons (▾ ⇄ 📁 📦 💾 ×) only by `title` |
| (manual) dialogs | — | 4 modals | no `role="dialog"`, no `aria-modal`, focus stays on the opener, no focus trap, **Escape does not close** (verified) |
| (manual) keyboard | — | change list | rows are clickable `div`s without `tabindex`; a change cannot be selected without a mouse; no arrow-key navigation; focus ring is the browser default |
| (manual) colour as the only signal | — | tree, diff | add/alter/drop are distinguished by glyph + colour (ok); hazard meaning only on hover (`title`) |

## 4. Heuristic evaluation

| # | Heuristic | Findings | Sev. |
|---|---|---|---|
| 1 | Visibility of system status | Compare progress dialog narrates phases and object counts (good). Publish progress streams per statement (good). But the results screen never says *when* the comparison ran or against which snapshot; the engine note (dev builds only) is styled exactly like a real warning. | 2 |
| 2 | Match with the real world | Warnings and hazards speak engine/CLI: "set compare.columnOrder: rebuild", "pg-dac publish --from <project> --to <target>", `ACQUIRES_ACCESS_EXCLUSIVE_LOCK`, `DELETES_DATA` "affects 2 object(s)" with no way to see which two. Option labels ("Routine whitespace", "project default (warn)") need the tooltip to be understood, and tooltips are hover-only. The severity badge says "error" on changes that will apply fine once acknowledged — it is a *risk*, not an error. | 3 |
| 3 | User control and freedom | Compare can be cancelled (good). Escape closes nothing. Warnings cannot be dismissed or collapsed. There is no way to re-open the last script or report after closing the dialog. | 3 |
| 4 | Consistency and standards | Deployment switches are split: Online, Drop objects, Allow drift, Skip deploy scripts and Deploy variables live in the *compare* Options; Single transaction lives in the Publish dialog. Monaco uses a light theme (`vs`) inside a dark UI. Icon buttons are emoji, rendered differently per OS. The primary action moves between "Publish to Target…" and "Update Target project…" (good) but the footer button's accessible name is its tooltip. | 3 |
| 5 | Error prevention | Strong: data-loss and error-hazard acknowledgement gates, dependency gating of excluded objects, drift refusal, missing-role check. Weakness: "Apply" is disabled without saying why until the user notices the checkboxes; "Drop objects not in source" is a one-click compare option with no confirmation of *what* would be dropped before publish. | 2 |
| 6 | Recognition rather than recall | 12 options in one wrapping row; their meaning is in `title` attributes only. Saved sources are a hidden ▾ menu; nothing on the empty screen tells a first-time user that saved sources exist. Hazard explanations are hover-only. | 3 |
| 7 | Flexibility and efficiency | No keyboard shortcuts (Compare, Publish, next/previous change). No search or filter beyond "hazards only"; no select-all/none; no grouping (the README says "grouped", the list is flat) — a 900-object schema with 50 changes is a flat scroll. Options are not remembered per saved source. | 3 |
| 8 | Aesthetic and minimalist design | The permanent 106 px warnings block and the 265 px source/options block starve the workspace (section 2). Three type sizes below 12 px. The direction banner, the tree toolbar and the status bar repeat counts in three places. | 3 |
| 9 | Help users recognise and recover from errors | Compare errors are a plain red banner with the raw daemon message; the publish log is an unstyled `pre`; a failed statement is not linked back to its change. | 2 |
| 10 | Help and documentation | None in the app; the only guidance is the README. The SSDT orientation (Source left = truth, Target right = updated) is explained by 10 px hints. | 2 |

## 5. Cognitive walkthrough — "publish the project to DEV"

1. **Pick sources.** Two identical text boxes; the user must know that ▾ hides saved sources
   and that 📁/📦 mean directory/artifact. The role hints ("of truth", "current · gets
   updated") are 10 px. *Friction: discoverability.*
2. **Decide options.** Options is collapsed by default (good) but, once opened, presents all 12
   controls at once with no grouping; five of them do not affect the comparison at all.
   *Friction: too many decisions too early, wrong place.*
3. **Compare.** The progress dialog is clear. *OK.*
4. **Understand the result.** The direction banner says "1 add · 3 change · 1 drop" (good).
   Three warnings in CLI vocabulary occupy the next 106 px and never go away. Rows show
   `error`/`warning` badges whose meaning is only revealed on the detail pane's hazard chips, and
   only on hover. *Friction: vocabulary, permanent noise.*
5. **Review a change.** The side-by-side diff with "Source (of truth) / Target (current)" and
   the "show result" toggle is the best part of the UI. But the editor is light-on-white inside a
   dark shell, and `NOT NULL` tokens fail contrast. Switching changes requires the mouse.
   *Friction: theme, keyboard.*
6. **Exclude an object.** Checkbox per row works; dependency gating is explained in a tooltip
   only. *Minor.*
7. **Publish.** The dialog explains the action in a paragraph, then lists gates. "DELETES_DATA
   affects 2 object(s)" — which two? The user has to close the dialog and look. Single
   transaction appears here while Online/Allow drift were back in Options. *Friction: split
   settings, no drill-down.*
8. **After publish.** "Applied 11 statement(s)." Close. No report to save, no record of what
   was done, the warnings banner is still there. *Friction: closure.*

## 6. Redesign options

Everything below shares one **foundation layer** that is required whichever option is chosen:

- Accessible names for every control (visible labels or `aria-label`), `role="dialog"` +
  `aria-modal` + focus trap + Escape on every modal, a focusable change list with arrow-key
  navigation and `aria-selected`, a visible focus ring, landmarks and a heading hierarchy.
- A design-token file (colours with ≥ 4.5:1 on their surfaces, a 12/13/14 px type scale, no
  10 px text), Monaco dark theme matching the shell, real icons (Lucide) instead of emoji.
- Human copy: hazard codes rendered as a title + one sentence + the affected objects; warnings
  grouped by side, collapsible and dismissible; the dev-only engine note demoted to the status
  bar.
- Terminology: "risk" (error/warning hazards) instead of "error"; consistent Source/Target.

### Option A — "SSDT faithful": restructure in place (≈ 1 week)

Keep one screen, re-rank what is on it. A toolbar row (Compare · Generate Script · Publish ·
Options ⚙ · ⇄) replaces the status-bar actions; Source/Target become two compact pickers
whose dropdown *is* the saved-source list (with "Browse…" entries inside it); **Options moves
to a dialog** with tabs *Comparison* (schemas, privileges, column order, whitespace, comments,
excluded object types) and *Deployment* (online, drop policy, drift, deploy scripts, variables,
single transaction, timeouts). The results list groups changes by action (Drop / Change / Add)
with a tri-state checkbox per group, and the warnings move into a collapsible panel under the
list with a count in the status bar. Workspace height goes from 345 px to ≈ 700 px at 1366×820.

- Pros: smallest change; familiar to SSDT users; all existing components survive.
- Cons: still a dense single screen; deployment settings remain one click away from the
  compare, not where the deploy decision is made.

### Option B — "Guided workflow": three stages, one window (≈ 2–3 weeks) — recommended

A slim left rail with three stages — **1 Sources**, **2 Review**, **3 Deploy** — each a full
height screen; moving forward is explicit, moving back is always allowed.

- *Sources*: Source and Target as two cards (saved connections as a list with kind icons,
  "Browse folder", "Open artifact", "Test connection"); the comparison options in grouped
  accordions with one-line plain-language descriptions and the project default shown as the
  resolved value ("warn — from acme pgdac.yaml"). Compare is the single primary action.
- *Review*: the whole window for the change list and the diff. A header line states "Source →
  Target · compared 14:02 · 915/914 objects". The list is a tree (schema → object type → object)
  or grouped by action (toggle), with search, filters (add/change/drop, risk level, hazard code),
  select all/none, and keyboard navigation. The detail pane has *Definition*, *Generated SQL*
  and *Risks* tabs; *Risks* explains each hazard. Warnings are a collapsible "Notes" drawer with
  a badge count. Footer: "Generate Script" and "Continue to Deploy".
- *Deploy*: a pre-flight page — what will be applied (counts, statements, estimated locks),
  risks to acknowledge (each with its objects listed), deployment settings (online, single
  transaction, timeouts, drift, deploy scripts and variables) with the saved-source defaults,
  then Publish with live progress and a result summary that can be saved as a report.

- Pros: each screen has one job and full height; options sit where their decision is made;
  first-time users are guided while experts can jump stages from the rail; maps 1:1 onto the
  CLI's plan → script → publish.
- Cons: more screens and state; the review pane loses the always-visible source pickers (they
  become a header line with "Change…").

### Option C — "IDE workbench": multi-session power tool (≈ 4–6 weeks)

A VS Code-style shell: an activity bar (Compare, Drift, Artifacts, Connections, Settings),
multiple compare sessions as tabs, a command palette with every action and shortcut, a true
object tree with inline diff decorations, a bottom panel with *Problems* (warnings and risks),
*Output* (publish log) and *Script*, and a searchable Settings page with per-connection
defaults. Exposes the CLI's other actions (drift report, inspect/export/import of artifacts) in
the GUI.

- Pros: the most complete product; parity with the CLI; scales to many environments.
- Cons: largest scope and the highest risk for a 0.9 release; much of it is new surface rather
  than fixing the current one.

### Recommendation

Do the foundation layer first (it is needed regardless and removes the critical/serious
accessibility failures), then Option B. Option A's toolbar and grouped list are a subset of B's
Review stage and can be delivered as its first milestone, so B does not block on finishing
everything at once. Option C's Problems panel and command palette are good follow-ups once B is
in place.

## 7. Evidence captured

Screenshots and the axe report were taken in the session on 2026-10-07 from
`pnpm --dir ui dev` at 1366×820 and 1024×680 with the browser mock (`src/ipc/mock.ts`,
fixture `src/dev/fixture-plan.json`). Re-run: start `pnpm dev` in `ui/`, open
http://localhost:1420 in a browser (the client routes to the mock when Tauri is absent), run
axe-core from the console.
