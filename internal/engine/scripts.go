package engine

import (
	"fmt"
	"strings"

	"github.com/rodrigonunes/pg-dac/internal/model"
)

// ObjectScript is one object's canonical CREATE SQL, the project section folder
// it belongs in, and a filename stem (unique within the section). Used by
// `extract --to-project` to lay a database out as a declarative project.
type ObjectScript struct {
	Section string
	Base    string
	SQL     string
}

// FileName returns a filesystem-safe "<base>.sql".
func (o ObjectScript) FileName() string { return sanitizeFilename(o.Base) + ".sql" }

// ObjectScripts renders a database as section-bucketed scripts in dependency
// order. Foreign keys, triggers, and policies are split out of their table into
// later sections so the one-object-per-file project applies cleanly; views are
// emitted in topological order; serial-backed sequences precede their table.
func ObjectScripts(db *model.Database) []ObjectScript {
	var out []ObjectScript
	emit := func(section, base, sql string) {
		if strings.TrimSpace(sql) != "" {
			out = append(out, ObjectScript{Section: section, Base: base, SQL: sql})
		}
	}

	for _, e := range db.Extensions {
		sql := "CREATE EXTENSION IF NOT EXISTS " + model.QuoteIdent(e.Name)
		if e.Schema != "" {
			sql += " SCHEMA " + model.QuoteIdent(e.Schema)
		}
		emit("01-extensions", e.Name, sql+";")
	}

	// ownedBy collects "ALTER SEQUENCE ... OWNED BY table.col" statements keyed by
	// owning table, appended to the table's script (the table must exist before
	// OWNED BY runs, while the sequence is created earlier).
	ownedBy := map[string][]string{}

	for _, s := range db.Schemas {
		if s.Name != "public" {
			emit("02-schemas", s.Name, "CREATE SCHEMA IF NOT EXISTS "+model.QuoteIdent(s.Name)+";")
		}
		for _, en := range s.Enums {
			emit("03-types", en.Schema+"."+en.Name, objectCanonical(db, en.Ref()))
		}
		for _, co := range s.Collations {
			emit("03-types", "collation."+co.Schema+"."+co.Name, objectCanonical(db, co.Ref()))
		}
		// Functions precede tables: a table default/trigger may reference one, and
		// PL/pgSQL bodies are not validated against table existence at creation.
		for _, f := range s.Functions {
			emit("04-functions", f.Schema+"."+f.Signature(), objectCanonical(db, f.Ref()))
		}
		for _, sq := range s.Sequences {
			if isIdentitySequence(db, sq) {
				continue // created implicitly with its GENERATED ... AS IDENTITY column
			}
			emit("05-sequences", sq.Schema+"."+sq.Name, sequenceCreateSQL(db, sq))
			if sq.OwnedBy != nil {
				tk := model.ObjRef{Kind: model.KindTable, Schema: sq.OwnedBy.Schema, Name: sq.OwnedBy.Table}.Key()
				ownedBy[tk] = append(ownedBy[tk],
					"ALTER SEQUENCE "+model.QualifiedName(sq.Schema, sq.Name)+
						" OWNED BY "+model.QualifiedName(sq.OwnedBy.Schema, sq.OwnedBy.Table)+
						"."+model.QuoteIdent(sq.OwnedBy.Column)+";")
			}
		}
		for _, t := range s.Tables {
			sql := tableCreateSQL(db, t)
			if extra := ownedBy[t.Ref().Key()]; len(extra) > 0 {
				sql += "\n" + strings.Join(extra, "\n")
			}
			emit("06-tables", t.Schema+"."+t.Name, sql)
		}
		// One file per table holding all its foreign keys (applied after all tables).
		for _, t := range s.Tables {
			var fks []string
			for _, c := range t.Constraints {
				if c.Type == model.ConstraintForeignKey {
					fks = append(fks, "ALTER TABLE "+t.QualifiedName()+" ADD CONSTRAINT "+model.QuoteIdent(c.Name)+" "+c.Definition+";")
				}
			}
			emit("07-foreign-keys", t.Schema+"."+t.Name, strings.Join(fks, "\n"))
		}
		for _, t := range s.Tables {
			for _, tr := range t.Triggers {
				emit("08-triggers", t.Schema+"."+t.Name+"."+tr.Name, terminateSQL(tr.Definition))
			}
		}
		for _, t := range s.Tables {
			for _, p := range t.Policies {
				emit("09-policies", t.Schema+"."+t.Name+"."+p.Name, policyCreateSQL(db, t, p))
			}
		}
		// Views/matviews in topological order (view-on-view), numbered so the
		// project's alphabetical glob preserves that order.
		for i, ref := range orderedViewRefs(s) {
			emit("10-views", fmt.Sprintf("%04d_%s.%s", i+1, ref.Schema, ref.Name), objectCanonical(db, ref))
		}
	}
	return out
}

// orderedViewRefs returns the schema's views and materialized views in
// dependency order (a view is emitted after the views/matviews it references).
func orderedViewRefs(s *model.Schema) []model.ObjRef {
	deps := map[string][]string{}
	refByKey := map[string]model.ObjRef{}
	var all []model.ObjRef
	record := func(ref model.ObjRef, dependsOn []model.ObjRef) {
		all = append(all, ref)
		refByKey[ref.Key()] = ref
		for _, d := range dependsOn {
			deps[ref.Key()] = append(deps[ref.Key()], d.Key())
		}
	}
	for _, v := range s.Views {
		record(v.Ref(), v.DependsOn)
	}
	for _, mv := range s.MatViews {
		record(mv.Ref(), mv.DependsOn)
	}

	var order []model.ObjRef
	state := map[string]int{} // 0 unvisited, 1 visiting, 2 done
	var visit func(k string)
	visit = func(k string) {
		if state[k] != 0 {
			return // done, or a cycle we break by not recursing
		}
		state[k] = 1
		for _, d := range deps[k] {
			if _, ok := refByKey[d]; ok {
				visit(d)
			}
		}
		state[k] = 2
		order = append(order, refByKey[k])
	}
	for _, ref := range all {
		visit(ref.Key())
	}
	return order
}

// tableCreateSQL renders a table without its foreign keys, triggers, or policies
// (those go to later sections), keeping columns, non-FK constraints, indexes, RLS.
func tableCreateSQL(db *model.Database, t *model.Table) string {
	cp := *t
	cp.Triggers = nil
	cp.Policies = nil
	cp.Constraints = nil
	for _, c := range t.Constraints {
		if c.Type != model.ConstraintForeignKey {
			cp.Constraints = append(cp.Constraints, c)
		}
	}
	mini := &model.Database{TargetVersion: db.TargetVersion,
		Schemas: []*model.Schema{{Name: t.Schema, Tables: []*model.Table{&cp}}}}
	baseline := &model.Database{TargetVersion: db.TargetVersion,
		Schemas: []*model.Schema{{Name: t.Schema}}}
	return joinSQL(Compare(baseline, mini, Options{Online: false}))
}

// policyCreateSQL renders just the CREATE POLICY for p by diffing the table with
// and without it (RLS enable is already emitted with the table, so it is held
// constant on both sides).
func policyCreateSQL(db *model.Database, t *model.Table, p *model.Policy) string {
	base := *t
	base.Triggers, base.Policies = nil, nil
	with := base
	with.Policies = []*model.Policy{p}
	miniDB := func(tb model.Table) *model.Database {
		return &model.Database{TargetVersion: db.TargetVersion,
			Schemas: []*model.Schema{{Name: t.Schema, Tables: []*model.Table{&tb}}}}
	}
	return joinSQL(Compare(miniDB(base), miniDB(with), Options{Online: false}))
}

func joinSQL(res *Result, err error) string {
	if err != nil || res.Empty() {
		return ""
	}
	parts := make([]string, 0, len(res.Statements))
	for _, s := range res.Statements {
		parts = append(parts, s.SQL)
	}
	return strings.Join(parts, "\n")
}

func terminateSQL(sql string) string {
	s := strings.TrimRight(strings.TrimSpace(sql), ";")
	if s == "" {
		return ""
	}
	return s + ";"
}

// sequenceCreateSQL renders a bare CREATE SEQUENCE. It clears OwnedBy on a copy
// because the differ deliberately skips owned sequences; ownership is restored
// separately by an ALTER SEQUENCE ... OWNED BY on the owning table's script.
func sequenceCreateSQL(db *model.Database, sq *model.Sequence) string {
	cp := *sq
	cp.OwnedBy = nil
	mini := &model.Database{TargetVersion: db.TargetVersion,
		Schemas: []*model.Schema{{Name: sq.Schema, Sequences: []*model.Sequence{&cp}}}}
	baseline := &model.Database{TargetVersion: db.TargetVersion,
		Schemas: []*model.Schema{{Name: sq.Schema}}}
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

// isIdentitySequence reports whether a sequence is the implicit sequence of a
// GENERATED ... AS IDENTITY column (which must not be emitted standalone, since
// the identity column re-creates it). Serial-backed sequences return false.
func isIdentitySequence(db *model.Database, seq *model.Sequence) bool {
	if seq.OwnedBy == nil {
		return false
	}
	for _, s := range db.Schemas {
		if s.Name != seq.OwnedBy.Schema {
			continue
		}
		for _, t := range s.Tables {
			if t.Name != seq.OwnedBy.Table {
				continue
			}
			for _, c := range t.Columns {
				if c.Name == seq.OwnedBy.Column {
					return c.Identity != ""
				}
			}
		}
	}
	return false
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
