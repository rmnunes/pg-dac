package refactor

import (
	"regexp"
	"strings"

	"github.com/rmnunes/pg-dac/internal/model"
)

// ApplyToModel returns a copy of the target model with the pending renames
// applied, so a comparison against the desired state sees renamed objects
// instead of a drop + create pair. Names, dependency references and the
// canonical definitions that embed the old name (an index's ON clause, a
// foreign key's REFERENCES, a nextval() default, ...) are updated the way
// PostgreSQL's own deparser would show them after the real ALTER ... RENAME.
// Definitions this cannot see into (a view body) are left alone: the differ
// then emits a harmless CREATE OR REPLACE for them, which converges.
func ApplyToModel(target *model.Database, renames []Rename) *model.Database {
	if len(renames) == 0 {
		return target
	}
	out := clone(target)
	for _, r := range renames {
		applyOp(out, r.Op)
	}
	out.Sort()
	return out
}

// clone deep-copies a model through its canonical JSON (every field is
// serializable); the copy is sorted like any freshly unmarshaled model.
func clone(db *model.Database) *model.Database {
	data, err := db.Marshal()
	if err != nil {
		return db
	}
	cp, err := model.Unmarshal(data)
	if err != nil {
		return db
	}
	return cp
}

func applyOp(db *model.Database, op Op) {
	toSchema := op.ToSchema
	if toSchema == "" {
		toSchema = op.Schema
	}
	switch op.Kind {
	case KindSchema:
		renameSchema(db, op.From, op.To)
	case KindTable:
		renameTable(db, op.Schema, op.From, toSchema, op.To)
	case KindColumn:
		renameColumn(db, op.Schema, op.Table, op.From, op.To)
	case KindView:
		renameView(db, op.Schema, op.From, toSchema, op.To)
	case KindMatView:
		renameMatView(db, op.Schema, op.From, toSchema, op.To)
	case KindSequence:
		renameSequence(db, op.Schema, op.From, toSchema, op.To)
	case KindFunction:
		renameFunction(db, op.Schema, op.From, toSchema, op.To)
	case KindEnum, KindDomain, KindComposite:
		renameType(db, op.Kind, op.Schema, op.From, toSchema, op.To)
	case KindIndex:
		renameIndex(db, op.Schema, op.From, op.To)
	case KindConstraint:
		if t := db.Table(op.Schema, op.Table); t != nil {
			for _, c := range t.Constraints {
				if c.Name == op.From {
					c.Name = op.To
				}
			}
		}
	case KindTrigger:
		for _, tr := range triggersOf(db, op.Schema, op.Table) {
			if tr.Name == op.From {
				tr.Name = op.To
				tr.Definition = replaceAfter(tr.Definition, "TRIGGER ", op.From, op.To)
			}
		}
	case KindPolicy:
		if t := db.Table(op.Schema, op.Table); t != nil {
			for _, p := range t.Policies {
				if p.Name == op.From {
					p.Name = op.To
				}
			}
		}
	}
}

// --- object renames ---

func renameTable(db *model.Database, schema, from, toSchema, to string) {
	t := db.Table(schema, from)
	if t == nil {
		return
	}
	t.Schema, t.Name = toSchema, to
	for _, ix := range t.Indexes {
		ix.Table = to
		ix.Definition = replaceQualified(ix.Definition, schema, from, toSchema, to)
	}
	for _, tr := range t.Triggers {
		tr.Table = to
		tr.Definition = replaceQualified(tr.Definition, schema, from, toSchema, to)
	}
	for _, p := range t.Policies {
		p.Table = to
	}
	retargetRelation(db, model.KindTable, schema, from, toSchema, to)
	for _, s := range db.Schemas {
		for _, sq := range s.Sequences {
			if sq.OwnedBy != nil && sq.OwnedBy.Schema == schema && sq.OwnedBy.Table == from {
				sq.OwnedBy.Schema, sq.OwnedBy.Table = toSchema, to
			}
		}
		for _, other := range s.Tables {
			for _, c := range other.Constraints {
				if c.Type == model.ConstraintForeignKey && c.RefSchema == schema && c.RefTable == from {
					c.RefSchema, c.RefTable = toSchema, to
					c.Definition = replaceQualified(c.Definition, schema, from, toSchema, to)
				}
			}
			if other.PartitionOf != nil && other.PartitionOf.Schema == schema && other.PartitionOf.Table == from {
				other.PartitionOf.Schema, other.PartitionOf.Table = toSchema, to
			}
		}
	}
}

func renameView(db *model.Database, schema, from, toSchema, to string) {
	v := findView(db, schema, from)
	if v == nil {
		return
	}
	v.Schema, v.Name = toSchema, to
	for _, tr := range v.Triggers {
		tr.Table = to
		tr.Definition = replaceQualified(tr.Definition, schema, from, toSchema, to)
	}
	retargetRelation(db, model.KindView, schema, from, toSchema, to)
}

func renameMatView(db *model.Database, schema, from, toSchema, to string) {
	mv := findMatView(db, schema, from)
	if mv == nil {
		return
	}
	mv.Schema, mv.Name = toSchema, to
	for _, ix := range mv.Indexes {
		ix.Table = to
		ix.Definition = replaceQualified(ix.Definition, schema, from, toSchema, to)
	}
	retargetRelation(db, model.KindMatView, schema, from, toSchema, to)
}

// retargetRelation updates every dependency reference (and column-level
// reference) that points at a renamed relation.
func retargetRelation(db *model.Database, kind model.ObjKind, schema, from, toSchema, to string) {
	fix := func(refs []model.ObjRef) {
		for i := range refs {
			if refs[i].Kind == kind && refs[i].Schema == schema && refs[i].Name == from {
				refs[i].Schema, refs[i].Name = toSchema, to
			}
		}
	}
	fixCols := func(refs []model.ColumnRef) {
		for i := range refs {
			if refs[i].Schema == schema && refs[i].Table == from {
				refs[i].Schema, refs[i].Table = toSchema, to
			}
		}
	}
	for _, s := range db.Schemas {
		for _, v := range s.Views {
			fix(v.DependsOn)
			fixCols(v.ColumnDeps)
		}
		for _, mv := range s.MatViews {
			fix(mv.DependsOn)
			fixCols(mv.ColumnDeps)
		}
		for _, f := range s.Functions {
			fix(f.DependsOn)
		}
	}
}

func renameColumn(db *model.Database, schema, table, from, to string) {
	t := db.Table(schema, table)
	if t == nil {
		return
	}
	c := t.Column(from)
	if c == nil {
		return
	}
	c.Name = to
	for _, other := range t.Columns {
		other.Default = replaceIdent(other.Default, from, to)
		other.GeneratedExpr = replaceIdent(other.GeneratedExpr, from, to)
	}
	for _, con := range t.Constraints {
		for i := range con.Columns {
			if con.Columns[i] == from {
				con.Columns[i] = to
			}
		}
		if con.Type == model.ConstraintForeignKey {
			// Only the local column list may mention it; the REFERENCES part names
			// the other table's columns.
			if p := strings.Index(con.Definition, " REFERENCES "); p > 0 {
				con.Definition = replaceIdent(con.Definition[:p], from, to) + con.Definition[p:]
				continue
			}
		}
		con.Definition = replaceIdent(con.Definition, from, to)
	}
	for _, ix := range t.Indexes {
		ix.Definition = replaceAfter(ix.Definition, " USING ", "", "") // no-op placeholder keeps the structure explicit
		ix.Definition = replaceIdentAfter(ix.Definition, " USING ", from, to)
		for i := range ix.Columns {
			if ix.Columns[i] == from {
				ix.Columns[i] = to
			}
		}
	}
	for _, p := range t.Policies {
		p.Using = replaceIdent(p.Using, from, to)
		p.WithCheck = replaceIdent(p.WithCheck, from, to)
	}
	for _, s := range db.Schemas {
		for _, sq := range s.Sequences {
			if sq.OwnedBy != nil && sq.OwnedBy.Schema == schema && sq.OwnedBy.Table == table && sq.OwnedBy.Column == from {
				sq.OwnedBy.Column = to
			}
		}
		for _, other := range s.Tables {
			for _, con := range other.Constraints {
				if con.Type == model.ConstraintForeignKey && con.RefSchema == schema && con.RefTable == table {
					for i := range con.RefColumns {
						if con.RefColumns[i] == from {
							con.RefColumns[i] = to
						}
					}
					if p := strings.Index(con.Definition, " REFERENCES "); p > 0 {
						con.Definition = con.Definition[:p] + replaceIdent(con.Definition[p:], from, to)
					}
				}
			}
		}
		for _, v := range s.Views {
			for i := range v.ColumnDeps {
				if v.ColumnDeps[i].Schema == schema && v.ColumnDeps[i].Table == table && v.ColumnDeps[i].Column == from {
					v.ColumnDeps[i].Column = to
				}
			}
		}
		for _, mv := range s.MatViews {
			for i := range mv.ColumnDeps {
				if mv.ColumnDeps[i].Schema == schema && mv.ColumnDeps[i].Table == table && mv.ColumnDeps[i].Column == from {
					mv.ColumnDeps[i].Column = to
				}
			}
		}
	}
}

func renameSequence(db *model.Database, schema, from, toSchema, to string) {
	s := findSchema(db, schema)
	if s == nil {
		return
	}
	for _, sq := range s.Sequences {
		if sq.Name == from {
			sq.Schema, sq.Name = toSchema, to
		}
	}
	for _, sc := range db.Schemas {
		for _, t := range sc.Tables {
			for _, c := range t.Columns {
				c.Default = replaceQualified(c.Default, schema, from, toSchema, to)
			}
		}
	}
	retargetRelation(db, model.KindSequence, schema, from, toSchema, to)
}

func renameFunction(db *model.Database, schema, fromSig, toSchema, to string) {
	s := findSchema(db, schema)
	if s == nil {
		return
	}
	fromName, args := splitSignature(fromSig)
	toSig := to + args
	for _, f := range s.Functions {
		if f.Signature() == fromSig {
			f.Schema, f.Name = toSchema, to
			f.Definition = replaceQualified(f.Definition, schema, fromName, toSchema, to)
		}
	}
	for _, sc := range db.Schemas {
		for _, v := range sc.Views {
			for i := range v.DependsOn {
				if v.DependsOn[i].Kind == model.KindFunction && v.DependsOn[i].Schema == schema && v.DependsOn[i].Name == fromSig {
					v.DependsOn[i].Schema, v.DependsOn[i].Name = toSchema, toSig
				}
			}
			for _, tr := range v.Triggers {
				retargetTrigger(tr, schema, fromSig, fromName, toSchema, to, toSig)
			}
		}
		for _, mv := range sc.MatViews {
			for i := range mv.DependsOn {
				if mv.DependsOn[i].Kind == model.KindFunction && mv.DependsOn[i].Schema == schema && mv.DependsOn[i].Name == fromSig {
					mv.DependsOn[i].Schema, mv.DependsOn[i].Name = toSchema, toSig
				}
			}
		}
		for _, t := range sc.Tables {
			for _, tr := range t.Triggers {
				retargetTrigger(tr, schema, fromSig, fromName, toSchema, to, toSig)
			}
		}
		for _, f := range sc.Functions {
			for i := range f.DependsOn {
				if f.DependsOn[i].Kind == model.KindFunction && f.DependsOn[i].Schema == schema && f.DependsOn[i].Name == fromSig {
					f.DependsOn[i].Schema, f.DependsOn[i].Name = toSchema, toSig
				}
			}
		}
	}
}

func retargetTrigger(tr *model.Trigger, schema, fromSig, fromName, toSchema, to, toSig string) {
	for i := range tr.DependsOn {
		if tr.DependsOn[i].Kind == model.KindFunction && tr.DependsOn[i].Schema == schema && tr.DependsOn[i].Name == fromSig {
			tr.DependsOn[i].Schema, tr.DependsOn[i].Name = toSchema, toSig
			tr.Definition = replaceQualified(tr.Definition, schema, fromName, toSchema, to)
		}
	}
}

func renameType(db *model.Database, kind Kind, schema, from, toSchema, to string) {
	s := findSchema(db, schema)
	if s == nil {
		return
	}
	var refKind model.ObjKind
	switch kind {
	case KindEnum:
		refKind = model.KindEnum
		for _, e := range s.Enums {
			if e.Name == from {
				e.Schema, e.Name = toSchema, to
			}
		}
	case KindDomain:
		refKind = model.KindDomain
		for _, d := range s.Domains {
			if d.Name == from {
				d.Schema, d.Name = toSchema, to
				d.Definition = replaceQualified(d.Definition, schema, from, toSchema, to)
			}
		}
	case KindComposite:
		refKind = model.KindComposite
		for _, c := range s.Composites {
			if c.Name == from {
				c.Schema, c.Name = toSchema, to
				c.Definition = replaceQualified(c.Definition, schema, from, toSchema, to)
			}
		}
	}
	oldQ, newQ := schema+"."+from, toSchema+"."+to
	for _, sc := range db.Schemas {
		for _, t := range sc.Tables {
			for _, c := range t.Columns {
				switch c.Type {
				case oldQ:
					c.Type = newQ
				case oldQ + "[]":
					c.Type = newQ + "[]"
				}
			}
		}
		for _, f := range sc.Functions {
			f.IdentityArgs = replaceQualified(f.IdentityArgs, schema, from, toSchema, to)
			f.Arguments = replaceQualified(f.Arguments, schema, from, toSchema, to)
			f.ReturnType = replaceQualified(f.ReturnType, schema, from, toSchema, to)
			f.Definition = replaceQualified(f.Definition, schema, from, toSchema, to)
			for i := range f.DependsOn {
				if f.DependsOn[i].Kind == refKind && f.DependsOn[i].Schema == schema && f.DependsOn[i].Name == from {
					f.DependsOn[i].Schema, f.DependsOn[i].Name = toSchema, to
				}
			}
		}
		for _, v := range sc.Views {
			for i := range v.Columns {
				if v.Columns[i].Type == oldQ {
					v.Columns[i].Type = newQ
				}
			}
		}
	}
}

func renameIndex(db *model.Database, schema, from, to string) {
	s := findSchema(db, schema)
	if s == nil {
		return
	}
	fix := func(ix *model.Index) {
		if ix.Name == from {
			ix.Name = to
			ix.Definition = replaceAfter(ix.Definition, "INDEX ", from, to)
		}
	}
	for _, t := range s.Tables {
		for _, ix := range t.Indexes {
			fix(ix)
		}
	}
	for _, mv := range s.MatViews {
		for _, ix := range mv.Indexes {
			fix(ix)
		}
	}
}

func renameSchema(db *model.Database, from, to string) {
	for _, s := range db.Schemas {
		if s.Name == from {
			s.Name = to
		}
	}
	fixRefs := func(refs []model.ObjRef) {
		for i := range refs {
			if refs[i].Schema == from {
				refs[i].Schema = to
			}
		}
	}
	fixCols := func(refs []model.ColumnRef) {
		for i := range refs {
			if refs[i].Schema == from {
				refs[i].Schema = to
			}
		}
	}
	for _, s := range db.Schemas {
		for _, t := range s.Tables {
			if t.Schema == from {
				t.Schema = to
			}
			for _, c := range t.Columns {
				c.Type = replacePrefix(c.Type, from, to)
				c.Default = replacePrefix(c.Default, from, to)
				c.GeneratedExpr = replacePrefix(c.GeneratedExpr, from, to)
				c.Collation = replacePrefix(c.Collation, from, to)
			}
			for _, con := range t.Constraints {
				if con.RefSchema == from {
					con.RefSchema = to
				}
				con.Definition = replacePrefix(con.Definition, from, to)
			}
			for _, ix := range t.Indexes {
				ix.Definition = replacePrefix(ix.Definition, from, to)
			}
			for _, tr := range t.Triggers {
				tr.Definition = replacePrefix(tr.Definition, from, to)
				fixRefs(tr.DependsOn)
			}
			for _, p := range t.Policies {
				p.Using = replacePrefix(p.Using, from, to)
				p.WithCheck = replacePrefix(p.WithCheck, from, to)
			}
			if t.PartitionOf != nil && t.PartitionOf.Schema == from {
				t.PartitionOf.Schema = to
			}
		}
		for _, v := range s.Views {
			if v.Schema == from {
				v.Schema = to
			}
			v.Definition = replacePrefix(v.Definition, from, to)
			fixRefs(v.DependsOn)
			fixCols(v.ColumnDeps)
			for _, tr := range v.Triggers {
				tr.Definition = replacePrefix(tr.Definition, from, to)
				fixRefs(tr.DependsOn)
			}
		}
		for _, mv := range s.MatViews {
			if mv.Schema == from {
				mv.Schema = to
			}
			mv.Definition = replacePrefix(mv.Definition, from, to)
			fixRefs(mv.DependsOn)
			fixCols(mv.ColumnDeps)
			for _, ix := range mv.Indexes {
				ix.Definition = replacePrefix(ix.Definition, from, to)
			}
		}
		for _, sq := range s.Sequences {
			if sq.Schema == from {
				sq.Schema = to
			}
			if sq.OwnedBy != nil && sq.OwnedBy.Schema == from {
				sq.OwnedBy.Schema = to
			}
		}
		for _, f := range s.Functions {
			if f.Schema == from {
				f.Schema = to
			}
			f.Definition = replacePrefix(f.Definition, from, to)
			f.IdentityArgs = replacePrefix(f.IdentityArgs, from, to)
			f.Arguments = replacePrefix(f.Arguments, from, to)
			f.ReturnType = replacePrefix(f.ReturnType, from, to)
			fixRefs(f.DependsOn)
		}
		for _, e := range s.Enums {
			if e.Schema == from {
				e.Schema = to
			}
		}
		for _, d := range s.Domains {
			if d.Schema == from {
				d.Schema = to
			}
			d.Definition = replacePrefix(d.Definition, from, to)
		}
		for _, c := range s.Composites {
			if c.Schema == from {
				c.Schema = to
			}
			c.Definition = replacePrefix(c.Definition, from, to)
		}
		for _, c := range s.Collations {
			if c.Schema == from {
				c.Schema = to
			}
			c.Definition = replacePrefix(c.Definition, from, to)
		}
	}
	for _, e := range db.Extensions {
		if e.Schema == from {
			e.Schema = to
		}
	}
}

// --- text helpers (bounded to canonical definitions pg-dac itself produced) ---

const identChars = `A-Za-z0-9_`

// replaceIdent replaces an identifier token (bare or double-quoted) in text.
func replaceIdent(text, from, to string) string {
	if text == "" || from == to {
		return text
	}
	text = strings.ReplaceAll(text, `"`+from+`"`, `"`+to+`"`)
	re := regexp.MustCompile(`(^|[^` + identChars + `"])` + regexp.QuoteMeta(from) + `([^` + identChars + `"]|$)`)
	return re.ReplaceAllString(text, "${1}"+to+"${2}")
}

// replaceIdentAfter applies replaceIdent only to the part of text after the
// first occurrence of marker (e.g. the column list of an index definition).
func replaceIdentAfter(text, marker, from, to string) string {
	p := strings.Index(text, marker)
	if p < 0 {
		return replaceIdent(text, from, to)
	}
	return text[:p] + replaceIdent(text[p:], from, to)
}

// replaceAfter replaces the first "<marker><from>" occurrence with "<marker><to>",
// handling a double-quoted name as well.
func replaceAfter(text, marker, from, to string) string {
	if from == "" {
		return text
	}
	if strings.Contains(text, marker+`"`+from+`"`) {
		return strings.Replace(text, marker+`"`+from+`"`, marker+`"`+to+`"`, 1)
	}
	re := regexp.MustCompile(regexp.QuoteMeta(marker+from) + `([^` + identChars + `]|$)`)
	return re.ReplaceAllString(text, marker+to+"${1}")
}

// replaceQualified replaces a schema-qualified object name (schema.name, with
// either part optionally double-quoted) in text.
func replaceQualified(text, oldSchema, oldName, newSchema, newName string) string {
	if text == "" {
		return text
	}
	forms := [][2]string{
		{oldSchema + "." + oldName, newSchema + "." + newName},
		{`"` + oldSchema + `"."` + oldName + `"`, `"` + newSchema + `"."` + newName + `"`},
		{oldSchema + `."` + oldName + `"`, newSchema + `."` + newName + `"`},
		{`"` + oldSchema + `".` + oldName, `"` + newSchema + `".` + newName},
	}
	for _, f := range forms {
		re := regexp.MustCompile(`(^|[^` + identChars + `."])` + regexp.QuoteMeta(f[0]) + `([^` + identChars + `"]|$)`)
		text = re.ReplaceAllString(text, "${1}"+f[1]+"${2}")
	}
	return text
}

// replacePrefix replaces a schema qualifier "old." (bare or quoted) in text.
func replacePrefix(text, oldSchema, newSchema string) string {
	if text == "" || oldSchema == newSchema {
		return text
	}
	text = strings.ReplaceAll(text, `"`+oldSchema+`".`, `"`+newSchema+`".`)
	re := regexp.MustCompile(`(^|[^` + identChars + `."])` + regexp.QuoteMeta(oldSchema) + `\.`)
	return re.ReplaceAllString(text, "${1}"+newSchema+".")
}
