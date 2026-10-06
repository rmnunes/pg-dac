package engine

import (
	"strings"

	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/safety"
	"github.com/rodrigonunes/pg-dac/internal/sqlgen"
)

// PlanFormatVersion is the schema version of PlanDoc / the daemon API. The GUI
// checks compatibility against this before trusting a plan.
const PlanFormatVersion = 1

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
}

// HazardCount aggregates one hazard code across all changed objects.
type HazardCount struct {
	Code     safety.HazardType `json:"code"`
	Severity safety.Severity   `json:"severity"`
	Count    int               `json:"count"`
}

// PlanDoc builds the rich plan document.
func (r *Result) PlanDoc() PlanDoc {
	changes := r.ObjectChanges()
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
	return doc
}

// ObjectChanges rolls the per-op statement groups up into one record per
// top-level object, in dependency (plan) order.
func (r *Result) ObjectChanges() []ObjectChange {
	fromKeys := presentKeys(r.from)
	toKeys := presentKeys(r.to)

	var order []string
	byKey := map[string]*ObjectChange{}
	for _, g := range r.Groups {
		key := g.Op.Owner.Key()
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
		ref := model.ObjRef{Kind: oc.ObjectType, Schema: oc.Schema, Name: oc.Name}
		switch oc.ChangeKind {
		case "add":
			oc.NewDefinition = objectCanonical(r.to, ref)
			oc.DependsOn = dependsOnFor(r.to, ref)
		case "drop":
			oc.OldDefinition = objectCanonical(r.from, ref)
			oc.DependsOn = dependsOnFor(r.from, ref)
		default:
			oc.OldDefinition = objectCanonical(r.from, ref)
			oc.NewDefinition = objectCanonical(r.to, ref)
			oc.DependsOn = dependsOnFor(r.to, ref)
		}
		out = append(out, *oc)
	}
	return out
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
	return m
}

// objectCanonical renders the canonical CREATE statements for a single object by
// materializing it alone (against an empty same-named schema) and rendering the
// non-online SQL — reusing the whole engine so every object kind is covered.
// Returns "" for objects without a meaningful standalone definition (schemas,
// extensions) or when the object is absent from db.
func objectCanonical(db *model.Database, ref model.ObjRef) string {
	if ref.Kind == model.KindSchema || ref.Kind == model.KindExtension {
		return ""
	}
	mini := extractObject(db, ref)
	if mini == nil {
		return ""
	}
	baseline := &model.Database{TargetVersion: db.TargetVersion, Schemas: []*model.Schema{{Name: ref.Schema}}}
	res, err := Compare(baseline, mini, Options{Online: false})
	if err != nil || res.Empty() {
		return ""
	}
	parts := make([]string, 0, len(res.Statements))
	for _, s := range res.Statements {
		parts = append(parts, s.SQL)
	}
	return strings.Join(parts, "\n")
}

// extractObject returns a one-object model (the object under its schema), or nil
// if the object is not found.
func extractObject(db *model.Database, ref model.ObjRef) *model.Database {
	if db == nil {
		return nil
	}
	var src *model.Schema
	for _, s := range db.Schemas {
		if s.Name == ref.Schema {
			src = s
			break
		}
	}
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
		for _, f := range src.Functions {
			if f.Ref().Key() == ref.Key() {
				out.Functions = []*model.Function{f}
			}
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
	var src *model.Schema
	for _, s := range db.Schemas {
		if s.Name == ref.Schema {
			src = s
		}
	}
	if src == nil {
		return deps
	}
	switch ref.Kind {
	case model.KindTable:
		for _, t := range src.Tables {
			if t.Name != ref.Name {
				continue
			}
			for _, c := range t.Constraints {
				if c.Type == model.ConstraintForeignKey && c.RefTable != "" {
					add(model.ObjRef{Kind: model.KindTable, Schema: c.RefSchema, Name: c.RefTable}.Key())
				}
			}
		}
	case model.KindView:
		for _, v := range src.Views {
			if v.Name == ref.Name {
				for _, d := range v.DependsOn {
					add(d.Key())
				}
			}
		}
	case model.KindMatView:
		for _, mv := range src.MatViews {
			if mv.Name == ref.Name {
				for _, d := range mv.DependsOn {
					add(d.Key())
				}
			}
		}
	case model.KindFunction:
		for _, f := range src.Functions {
			if f.Ref().Key() == ref.Key() {
				for _, d := range f.DependsOn {
					add(d.Key())
				}
			}
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
