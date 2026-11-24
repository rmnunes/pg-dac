package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// PlanFormatVersion is the schema version of PlanDoc / the daemon API. The GUI
// checks compatibility against this before trusting a plan.
const PlanFormatVersion = 2

// PlanDoc is the rich, GUI-grade representation of a comparison: one entry per
// changed top-level object (table, view, function, …), each carrying its
// generated SQL, hazards, before/after definition, and dependency edges. It is
// what the `plan --json` command and the daemon's /plan endpoint emit.
type PlanDoc struct {
	FormatVersion  int            `json:"formatVersion"`
	StatementCount int            `json:"statementCount"`
	DataLoss       bool           `json:"dataLoss"`
	Changes        []ObjectChange `json:"changes"`
	HazardSummary  []HazardCount  `json:"hazardSummary,omitempty"`
	// Prelude lists refactorlog renames that run before the migration.
	Prelude []sqlgen.Statement `json:"prelude,omitempty"`
}

// ObjectChange describes the change to a single top-level object. Sub-object
// operations (columns, constraints, indexes, triggers, policies) are rolled up
// under their owning table, matching the SSDT Schema Compare per-object model;
// include/exclude therefore operates per object via ID.
type ObjectChange struct {
	ID            string             `json:"id"` // model.ObjRef.Key() — stable across re-diffs
	ObjectType    model.ObjKind      `json:"objectType"`
	Schema        string             `json:"schema,omitempty"`
	Name          string             `json:"name"`
	ChangeKind    string             `json:"changeKind"` // add | drop | alter
	DependsOn     []string           `json:"dependsOn,omitempty"`
	OldDefinition string             `json:"oldDefinition,omitempty"`
	NewDefinition string             `json:"newDefinition,omitempty"`
	Statements    []sqlgen.Statement `json:"statements"`
	Hazards       []safety.Hazard    `json:"hazards,omitempty"` // distinct, by type
	Severity      safety.Severity    `json:"severity,omitempty"`
	// Reason says why the engine touches an object beyond the plain diff — a
	// dependent recreated because what it reads changes, a table rebuilt for
	// its column order, an object kept across a rebuild — so a change whose
	// definition text is unchanged can explain itself. Distinct op reasons,
	// joined with "; ".
	Reason string `json:"reason,omitempty"`
}

// HazardCount aggregates one hazard code across all changed objects.
type HazardCount struct {
	Code     safety.HazardType `json:"code"`
	Severity safety.Severity   `json:"severity"`
	Count    int               `json:"count"`
}

// PlanDoc builds the rich plan document.
func (r *Result) PlanDoc() (PlanDoc, error) {
	changes, err := r.ObjectChanges()
	if err != nil {
		return PlanDoc{}, err
	}
	doc := PlanDoc{
		FormatVersion:  PlanFormatVersion,
		StatementCount: len(r.Statements) + len(r.Prelude),
		DataLoss:       r.HasDataLoss(),
		Changes:        changes,
		Prelude:        r.Prelude,
	}

	type agg struct {
		sev   safety.Severity
		count int
	}
	m := map[safety.HazardType]*agg{}
	var codeOrder []safety.HazardType
	for _, c := range changes {
		for _, h := range c.Hazards {
			a, ok := m[h.Type]
			if !ok {
				a = &agg{sev: h.Severity}
				m[h.Type] = a
				codeOrder = append(codeOrder, h.Type)
			}
			a.count++
		}
	}
	for _, code := range codeOrder {
		doc.HazardSummary = append(doc.HazardSummary, HazardCount{Code: code, Severity: m[code].sev, Count: m[code].count})
	}
	return doc, nil
}

// ObjectChanges rolls the per-op statement groups up into one record per
// top-level object, in dependency (plan) order. It fails only when an object's
// definition cannot be rendered, which is a defect in the renderer, not in
// the input.
func (r *Result) ObjectChanges() ([]ObjectChange, error) {
	fromKeys := presentKeys(r.from)
	toKeys := presentKeys(r.to)

	var order []string
	byKey := map[string]*ObjectChange{}
	reasons := map[string][]string{}
	reowned := map[string]diff.Op{}
	for _, g := range r.Groups {
		key := g.Op.Owner.Key()
		if g.Op.Type == diff.OpAlterOwner {
			reowned[key] = g.Op
		}
		if reason := g.Op.Reason; reason != "" && !slices.Contains(reasons[key], reason) {
			reasons[key] = append(reasons[key], reason)
		}
		oc, ok := byKey[key]
		if !ok {
			oc = &ObjectChange{
				ID:         key,
				ObjectType: g.Op.Owner.Kind,
				Schema:     g.Op.Owner.Schema,
				Name:       g.Op.Owner.Name,
				ChangeKind: changeKind(key, fromKeys, toKeys),
			}
			byKey[key] = oc
			order = append(order, key)
		}
		oc.Statements = append(oc.Statements, g.Statements...)
	}

	out := make([]ObjectChange, 0, len(order))
	for _, key := range order {
		oc := byKey[key]
		oc.Hazards, oc.Severity = aggregateHazards(oc.Statements)
		oc.Reason = strings.Join(reasons[key], "; ")
		ref := model.ObjRef{Kind: oc.ObjectType, Schema: oc.Schema, Name: oc.Name}
		var err error
		switch oc.ChangeKind {
		case "add":
			if oc.NewDefinition, err = objectCanonical(r.to, ref); err != nil {
				return nil, err
			}
			oc.DependsOn = dependsOnFor(r.to, ref)
		case "drop":
			if oc.OldDefinition, err = objectCanonical(r.from, ref); err != nil {
				return nil, err
			}
			oc.DependsOn = dependsOnFor(r.from, ref)
		default:
			if oc.OldDefinition, err = objectCanonical(r.from, ref); err != nil {
				return nil, err
			}
			if oc.NewDefinition, err = objectCanonical(r.to, ref); err != nil {
				return nil, err
			}
			oc.DependsOn = dependsOnFor(r.to, ref)
			// Definitions do not carry owners; a change of owner shows as the
			// statement that sets each.
			if op, ok := reowned[key]; ok {
				oc.OldDefinition = withOwner(oc.OldDefinition, sqlgen.AlterOwnerSQL(ref, op.Function, op.PrevOwner))
				oc.NewDefinition = withOwner(oc.NewDefinition, sqlgen.AlterOwnerSQL(ref, op.Function, op.NewOwner))
			}
		}
		out = append(out, *oc)
	}
	return out, nil
}

// withOwner appends the statement that sets an object's owner to its
// definition.
func withOwner(def, stmt string) string {
	if def == "" {
		return stmt + "\n"
	}
	return strings.TrimRight(def, "\n") + "\n\n" + stmt + "\n"
}

func changeKind(key string, fromKeys, toKeys map[string]model.ObjKind) string {
	_, inFrom := fromKeys[key]
	_, inTo := toKeys[key]
	switch {
	case inTo && !inFrom:
		return "add"
	case inFrom && !inTo:
		return "drop"
	default:
		return "alter"
	}
}

func aggregateHazards(stmts []sqlgen.Statement) ([]safety.Hazard, safety.Severity) {
	var out []safety.Hazard
	seen := map[safety.HazardType]bool{}
	var worst safety.Severity
	for _, s := range stmts {
		for _, h := range s.Hazards {
			if !seen[h.Type] {
				seen[h.Type] = true
				out = append(out, h)
			}
			worst = maxSeverity(worst, h.Severity)
		}
	}
	return out, worst
}

func severityRank(s safety.Severity) int {
	switch s {
	case safety.SeverityError:
		return 3
	case safety.SeverityWarning:
		return 2
	case safety.SeverityInfo:
		return 1
	default:
		return 0
	}
}

func maxSeverity(a, b safety.Severity) safety.Severity {
	if severityRank(b) > severityRank(a) {
		return b
	}
	return a
}

// presentKeys returns the set of top-level object keys present in a model.
func presentKeys(db *model.Database) map[string]model.ObjKind {
	m := map[string]model.ObjKind{}
	if db == nil {
		return m
	}
	for _, e := range db.Extensions {
		m[model.ObjRef{Kind: model.KindExtension, Name: e.Name}.Key()] = model.KindExtension
	}
	for _, s := range db.Schemas {
		m[model.ObjRef{Kind: model.KindSchema, Name: s.Name}.Key()] = model.KindSchema
		for _, t := range s.Tables {
			m[t.Ref().Key()] = model.KindTable
		}
		for _, v := range s.Views {
			m[v.Ref().Key()] = model.KindView
		}
		for _, mv := range s.MatViews {
			m[mv.Ref().Key()] = model.KindMatView
		}
		for _, sq := range s.Sequences {
			m[sq.Ref().Key()] = model.KindSequence
		}
		for _, en := range s.Enums {
			m[en.Ref().Key()] = model.KindEnum
		}
		for _, d := range s.Domains {
			m[d.Ref().Key()] = model.KindDomain
		}
		for _, c := range s.Composites {
			m[c.Ref().Key()] = model.KindComposite
		}
		for _, co := range s.Collations {
			m[co.Ref().Key()] = model.KindCollation
		}
		for _, f := range s.Functions {
			m[f.Ref().Key()] = model.KindFunction
		}
	}
	for _, dp := range db.DefaultPrivileges {
		m[dp.Ref().Key()] = model.KindDefaultPrivilege
	}
	return m
}

// objectCanonical renders an object's definition as the statements that would
// create it from nothing — the text the GUI diffs side by side. Every kind has
// one: a schema is its CREATE SCHEMA with comment and grants, an extension its
// CREATE EXTENSION with the installed version noted (a version upgrade is
// otherwise invisible in the text), everything else the object under its
// schema.
func objectCanonical(db *model.Database, ref model.ObjRef) (string, error) {
	if db == nil {
		return "", nil
	}
	switch ref.Kind {
	case model.KindSchema:
		s := db.Schema(ref.Name)
		if s == nil {
			return "", nil
		}
		bare := &model.Schema{Name: s.Name, Owner: s.Owner, Comment: s.Comment, Grants: s.Grants}
		return renderFromNothing(
			&model.Database{TargetVersion: db.TargetVersion},
			&model.Database{TargetVersion: db.TargetVersion, Schemas: []*model.Schema{bare}})
	case model.KindDefaultPrivilege:
		// The entry as the statements that set it up from nothing.
		for _, dp := range db.DefaultPrivileges {
			if dp.Ref().Key() != ref.Key() {
				continue
			}
			var schemas []*model.Schema
			if dp.Schema != "" {
				schemas = []*model.Schema{{Name: dp.Schema}}
			}
			return renderFromNothing(
				&model.Database{TargetVersion: db.TargetVersion, Schemas: schemas},
				&model.Database{TargetVersion: db.TargetVersion, Schemas: schemas, DefaultPrivileges: []*model.DefaultPrivilege{dp}})
		}
		return "", nil
	case model.KindExtension:
		for _, e := range db.Extensions {
			if e.Name != ref.Name {
				continue
			}
			var schemas []*model.Schema
			if e.Schema != "" {
				schemas = []*model.Schema{{Name: e.Schema}}
			}
			out, err := renderFromNothing(
				&model.Database{TargetVersion: db.TargetVersion, Schemas: schemas},
				&model.Database{TargetVersion: db.TargetVersion, Schemas: schemas, Extensions: []*model.Extension{e}})
			if err == nil && out != "" && e.Version != "" {
				out += "\n-- version " + e.Version
			}
			return out, err
		}
		return "", nil
	}
	mini := extractObject(db, ref)
	if mini == nil {
		return "", nil
	}
	baseline := &model.Database{TargetVersion: db.TargetVersion, Schemas: []*model.Schema{{Name: ref.Schema}}}
	return renderFromNothing(baseline, mini)
}

// renderFromNothing renders the statements that take baseline to mini, joined
// as one text.
func renderFromNothing(baseline, mini *model.Database) (string, error) {
	res, err := Compare(baseline, mini, Options{Online: false})
	if err != nil {
		return "", fmt.Errorf("render definition: %w", err)
	}
	parts := make([]string, 0, len(res.Statements))
	for _, s := range res.Statements {
		parts = append(parts, s.SQL)
	}
	return strings.Join(parts, "\n"), nil
}

// extractObject returns a one-object model (the object under its schema), or nil
// if the object is not found.
func extractObject(db *model.Database, ref model.ObjRef) *model.Database {
	if db == nil {
		return nil
	}
	src := db.Schema(ref.Schema)
	if src == nil {
		return nil
	}
	out := &model.Schema{Name: ref.Schema}
	switch ref.Kind {
	case model.KindTable:
		for _, t := range src.Tables {
			if t.Name == ref.Name {
				out.Tables = []*model.Table{t}
			}
		}
	case model.KindView:
		for _, v := range src.Views {
			if v.Name == ref.Name {
				out.Views = []*model.View{v}
			}
		}
	case model.KindMatView:
		for _, mv := range src.MatViews {
			if mv.Name == ref.Name {
				out.MatViews = []*model.MaterializedView{mv}
			}
		}
	case model.KindSequence:
		for _, sq := range src.Sequences {
			if sq.Name == ref.Name {
				out.Sequences = []*model.Sequence{sq}
			}
		}
	case model.KindEnum:
		for _, en := range src.Enums {
			if en.Name == ref.Name {
				out.Enums = []*model.EnumType{en}
			}
		}
	case model.KindDomain:
		for _, d := range src.Domains {
			if d.Name == ref.Name {
				out.Domains = []*model.Domain{d}
			}
		}
	case model.KindComposite:
		for _, c := range src.Composites {
			if c.Name == ref.Name {
				out.Composites = []*model.CompositeType{c}
			}
		}
	case model.KindCollation:
		for _, co := range src.Collations {
			if co.Name == ref.Name {
				out.Collations = []*model.Collation{co}
			}
		}
	case model.KindFunction:
		if f := db.Function(ref.Schema, ref.Name); f != nil {
			out.Functions = []*model.Function{f}
		}
	}
	return &model.Database{TargetVersion: db.TargetVersion, Schemas: []*model.Schema{out}}
}

// dependsOnFor returns the object-level dependency edges the GUI uses for
// include/exclude gating: a table's foreign-key targets, a view/matview's
// referenced relations, and (for schema-scoped objects) their schema.
func dependsOnFor(db *model.Database, ref model.ObjRef) []string {
	var deps []string
	seen := map[string]bool{}
	add := func(k string) {
		if k != ref.Key() && !seen[k] {
			seen[k] = true
			deps = append(deps, k)
		}
	}
	if ref.Schema != "" {
		add(model.ObjRef{Kind: model.KindSchema, Name: ref.Schema}.Key())
	}
	src := db.Schema(ref.Schema)
	if src == nil {
		return deps
	}
	addAll := func(refs []model.ObjRef) {
		for _, d := range refs {
			add(d.Key())
		}
	}
	switch ref.Kind {
	case model.KindTable:
		if t := db.Table(ref.Schema, ref.Name); t != nil {
			for _, c := range t.Constraints {
				if c.Type == model.ConstraintForeignKey && c.RefTable != "" {
					add(model.ObjRef{Kind: model.KindTable, Schema: c.RefSchema, Name: c.RefTable}.Key())
				}
			}
		}
	case model.KindView:
		if v := db.View(ref.Schema, ref.Name); v != nil {
			addAll(v.DependsOn)
		}
	case model.KindMatView:
		if mv := db.MatView(ref.Schema, ref.Name); mv != nil {
			addAll(mv.DependsOn)
		}
	case model.KindFunction:
		if f := db.Function(ref.Schema, ref.Name); f != nil {
			addAll(f.DependsOn)
		}
	case model.KindSequence:
		for _, sq := range src.Sequences {
			if sq.Name == ref.Name && sq.OwnedBy != nil {
				add(model.ObjRef{Kind: model.KindTable, Schema: sq.OwnedBy.Schema, Name: sq.OwnedBy.Table}.Key())
			}
		}
	}
	return deps
}
