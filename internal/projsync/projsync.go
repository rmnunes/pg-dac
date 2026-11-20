// Package projsync updates a declarative project from a schema model — the
// reverse direction of a publish, SSDT's "Update project from database". It
// indexes the project's files by the object each statement defines or
// annotates, then, for every object the comparison reports, replaces that
// object's declarative statements in place with pg-dac's canonical rendering,
// writes a new file at the stable path for an object the project lacks, and
// removes the statements of an object the policy drops. An object whose file
// defines it imperatively (ALTER TABLE ... ADD COLUMN, DO blocks, DML) cannot
// be rewritten by any tool and is refused with directions. Nothing is written
// until Apply; Plan.Diff shows the file changes first.
package projsync

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/sqltext"
)

// statement is one statement of a project file, attributed to the owning
// object (a top-level object's key, or a routine group for overloads).
type statement struct {
	span        sqltext.Span
	head        sqltext.Head
	unit        string // owner unit key; "" when unattributed
	declarative bool
}

type file struct {
	abs   string
	rel   string // slash-separated, relative to the project root
	text  string
	stmts []statement
}

// Index is a project's files with every statement attributed to its object.
type Index struct {
	dir    string
	files  []*file
	byUnit map[string][]loc
}

type loc struct{ file, stmt int }

// Scan indexes the schema files of a project. m is the project's own model
// (what the files build to): it resolves what kind of relation a trigger, an
// index or a GRANT names, and which relation an index belongs to.
func Scan(dir string, man *project.Manifest, m *model.Database) (*Index, error) {
	paths, err := man.IncludeFiles(dir)
	if err != nil {
		return nil, err
	}
	idx := &Index{dir: dir, byUnit: map[string][]loc{}}
	res := newResolver(m)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		rel, _ := filepath.Rel(dir, p)
		f := &file{abs: p, rel: filepath.ToSlash(rel), text: string(data)}
		for _, sp := range sqltext.Spans(f.text) {
			h := sqltext.ParseHead(f.text[sp.Code:sp.End])
			st := statement{span: sp, head: h, declarative: h.Declarative}
			if unit, ok := res.unit(h); ok {
				st.unit = unit
			}
			f.stmts = append(f.stmts, st)
		}
		idx.files = append(idx.files, f)
		for si, st := range f.stmts {
			if st.unit != "" {
				idx.byUnit[st.unit] = append(idx.byUnit[st.unit], loc{len(idx.files) - 1, si})
			}
		}
	}
	return idx, nil
}

// Change is one file the update writes or removes.
type Change struct {
	Path   string `json:"path"`   // slash-separated, relative to the project root
	Action string `json:"action"` // create | modify | delete
	Old    string `json:"-"`
	New    string `json:"-"`
}

// Refusal names an object the update could not write and why.
type Refusal struct {
	Object string `json:"object"`
	Reason string `json:"reason"`
}

// Plan is the set of file changes an update makes, plus what it refused and the
// roles the project's manifest should list.
type Plan struct {
	Changes    []Change  `json:"changes"`
	Refused    []Refusal `json:"refused,omitempty"`
	RolesAdded []string  `json:"rolesAdded,omitempty"`
	dir        string
}

// Empty reports whether there is nothing to write.
func (p *Plan) Empty() bool { return len(p.Changes) == 0 && len(p.RolesAdded) == 0 }

// Build plans the update of the project (indexed by idx, built to from) toward
// the desired model to, given the engine's comparison res of from → to.
// Objects the comparison leaves alone keep their files byte for byte.
func Build(idx *Index, man *project.Manifest, from, to *model.Database, res *engine.Result) (*Plan, error) {
	plan := &Plan{dir: idx.dir}
	rendered := map[string]engine.ProjectFile{}
	for _, f := range engine.ProjectFiles(to) {
		rendered[f.Owner.Key()] = f
	}
	fromRendered := map[string]string{}
	for _, f := range engine.ProjectFiles(from) {
		fromRendered[f.Owner.Key()] = f.SQL
	}
	dropped := map[string]bool{}
	for _, op := range res.ChangeSet.Ops {
		if op.Type.IsDrop() {
			dropped[op.Owner.Key()] = true
		}
	}

	// Affected units, in plan order, de-duplicated (several overloads of one
	// routine name form one unit).
	var units []string
	seen := map[string]bool{}
	unitOf := map[string][]model.ObjRef{} // unit -> object refs in it (from ∪ to)
	changes, err := res.ObjectChanges()
	if err != nil {
		return nil, err
	}
	for _, oc := range changes {
		ref := model.ObjRef{Kind: oc.ObjectType, Schema: oc.Schema, Name: oc.Name}
		u := unitKey(ref)
		if !seen[u] {
			seen[u] = true
			units = append(units, u)
		}
	}
	collect := func(db *model.Database) {
		if db == nil {
			return
		}
		for _, s := range db.Schemas {
			for _, f := range s.Functions {
				u := unitKey(f.Ref())
				if seen[u] && !containsRef(unitOf[u], f.Ref()) {
					unitOf[u] = append(unitOf[u], f.Ref())
				}
			}
		}
	}
	collect(from)
	collect(to)

	edits := map[int]*fileEdit{} // file index -> pending edits
	for _, u := range units {
		refs := unitOf[u]
		if len(refs) == 0 {
			refs = []model.ObjRef{refOfUnit(u)}
		}
		// New content of the unit: every member present in the desired model,
		// plus members the project keeps because the policy does not drop them.
		var parts []string
		var path string
		existsInFrom := false
		for _, r := range refs {
			if f, ok := rendered[r.Key()]; ok {
				parts = append(parts, f.SQL)
				if path == "" {
					path = f.Path
				}
			} else if sql, ok := fromRendered[r.Key()]; ok && !dropped[r.Key()] {
				parts = append(parts, sql)
			}
			if _, ok := fromRendered[r.Key()]; ok {
				existsInFrom = true
			}
		}
		newSQL := strings.Join(parts, "\n\n")
		locs := idx.byUnit[u]
		if imp := idx.imperative(locs); imp != "" {
			plan.Refused = append(plan.Refused, Refusal{Object: describeUnit(u),
				Reason: "defined imperatively in " + imp + "; a migration-style definition cannot be rewritten in place — convert the project to declarative files (pg-dac extract --to-project into a new directory, keeping these scripts as pre/post-deploy) or edit the statement by hand"})
			continue
		}
		if len(locs) == 0 {
			if existsInFrom {
				plan.Refused = append(plan.Refused, Refusal{Object: describeUnit(u),
					Reason: "its defining statements could not be located in the project files (defined through a DO block or an unqualified name?); edit the project by hand"})
				continue
			}
			if newSQL == "" {
				continue
			}
			plan.Changes = append(plan.Changes, Change{Path: path, Action: "create", New: newSQL + "\n"})
			continue
		}
		// Replace the first statement's code with the new content, drop the rest.
		sort.Slice(locs, func(i, j int) bool {
			if locs[i].file != locs[j].file {
				return locs[i].file < locs[j].file
			}
			return locs[i].stmt < locs[j].stmt
		})
		for k, l := range locs {
			e := edits[l.file]
			if e == nil {
				e = &fileEdit{}
				edits[l.file] = e
			}
			if k == 0 {
				e.replace = append(e.replace, replacement{stmt: l.stmt, text: newSQL})
			} else {
				e.remove = append(e.remove, l.stmt)
			}
		}
	}

	for fi, e := range edits {
		f := idx.files[fi]
		text := applyEdits(f, e)
		switch {
		case !sqltext.HasCode(text):
			plan.Changes = append(plan.Changes, Change{Path: f.rel, Action: "delete", Old: f.text})
		case text != f.text:
			plan.Changes = append(plan.Changes, Change{Path: f.rel, Action: "modify", Old: f.text, New: text})
		}
	}
	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Path < plan.Changes[j].Path })

	have := map[string]bool{}
	for _, r := range man.Roles {
		have[r] = true
	}
	for _, r := range to.Grantees() {
		if !have[r] {
			plan.RolesAdded = append(plan.RolesAdded, r)
		}
	}
	return plan, nil
}

type replacement struct {
	stmt int
	text string
}

type fileEdit struct {
	replace []replacement
	remove  []int
}

// applyEdits rewrites a file's text: replaced statements keep the comments
// that lead into them; removed statements go entirely, except that the first
// statement of a file keeps its leading comments (the file header).
func applyEdits(f *file, e *fileEdit) string {
	kind := map[int]string{}
	repl := map[int]string{}
	for _, r := range e.replace {
		kind[r.stmt] = "replace"
		repl[r.stmt] = r.text
	}
	for _, s := range e.remove {
		kind[s] = "remove"
	}
	var b strings.Builder
	pos := 0
	for si, st := range f.stmts {
		switch kind[si] {
		case "replace":
			b.WriteString(f.text[pos:st.span.Code])
			b.WriteString(repl[si])
			pos = st.span.End
		case "remove":
			if si == 0 {
				b.WriteString(f.text[pos:st.span.Code])
			} else {
				b.WriteString(f.text[pos:st.span.Start])
			}
			pos = st.span.End
		}
	}
	b.WriteString(f.text[pos:])
	out := strings.TrimRight(b.String(), " \t\r\n")
	if out != "" {
		out += "\n"
	}
	return out
}

// imperative returns "file: statement" for the first statement of the unit
// that is not a declarative definition, or "".
func (idx *Index) imperative(locs []loc) string {
	for _, l := range locs {
		f := idx.files[l.file]
		st := f.stmts[l.stmt]
		if !st.declarative {
			return f.rel + ": " + strings.TrimSuffix(firstLine(f.text[st.span.Code:st.span.End]), ";")
		}
	}
	return ""
}

// Apply writes the plan's file changes under the project root.
func (p *Plan) Apply() error {
	for _, c := range p.Changes {
		abs := filepath.Join(p.dir, filepath.FromSlash(c.Path))
		switch c.Action {
		case "delete":
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("delete %s: %w", c.Path, err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(abs, []byte(c.New), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", c.Path, err)
			}
		}
	}
	if len(p.RolesAdded) > 0 {
		if err := project.AddRoles(p.dir, p.RolesAdded); err != nil {
			return err
		}
	}
	return nil
}

// Summary lists the planned changes one per line.
func (p *Plan) Summary() string {
	var b strings.Builder
	for _, c := range p.Changes {
		fmt.Fprintf(&b, "%-7s %s\n", c.Action, c.Path)
	}
	if len(p.RolesAdded) > 0 {
		fmt.Fprintf(&b, "%-7s %s: roles %s\n", "modify", project.ManifestName, strings.Join(p.RolesAdded, ", "))
	}
	return b.String()
}

// Diff renders the planned changes as a unified diff.
func (p *Plan) Diff() string {
	var b strings.Builder
	for _, c := range p.Changes {
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", c.Path, c.Path)
		b.WriteString(unifiedDiff(c.Old, c.New))
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

func containsRef(refs []model.ObjRef, r model.ObjRef) bool {
	for _, x := range refs {
		if x.Key() == r.Key() {
			return true
		}
	}
	return false
}
