package diff

import "github.com/rodrigonunes/pg-dac/internal/model"

// Diff computes the operations required to transform the "from" database
// (current/target state) into the "to" database (desired state).
//
// Drops for objects present in "from" but absent from "to" are always emitted
// here; whether they are actually applied is governed by deploy options
// (DropObjectsNotInSource etc.) applied downstream, not by the differ.
func Diff(from, to *model.Database) *ChangeSet {
	cs := &ChangeSet{}

	diffExtensions(cs, from, to)
	diffSchemas(cs, from, to)

	fromSchemas := indexSchemas(from)
	toSchemas := indexSchemas(to)

	// Per-schema object diffing. Iterate the union of schema names.
	for _, name := range unionKeys(fromSchemas, toSchemas) {
		fs := fromSchemas[name]
		ts := toSchemas[name]
		diffEnums(cs, fs, ts)
		diffCollations(cs, fs, ts)
		diffSequences(cs, fs, ts)
		diffFunctions(cs, fs, ts)
		diffTables(cs, fs, ts)
		diffViews(cs, fs, ts)
		diffMatViews(cs, fs, ts)
	}
	return cs
}

func diffFunctions(cs *ChangeSet, from, to *model.Schema) {
	fromF := indexFunctions(from)
	toF := indexFunctions(to)
	for sig, f := range toF {
		prev, ok := fromF[sig]
		if !ok || prev.Definition != f.Definition {
			// CREATE OR REPLACE covers both create and body change.
			cs.add(Op{Type: OpCreateFunction, Owner: f.Ref(), Sub: f.IdentityArgs, Function: f})
		}
	}
	for sig, f := range fromF {
		if _, ok := toF[sig]; !ok {
			cs.add(Op{Type: OpDropFunction, Owner: f.Ref(), Sub: f.IdentityArgs, Function: f})
		}
	}
}

func diffExtensions(cs *ChangeSet, from, to *model.Database) {
	fromExt := map[string]*model.Extension{}
	for _, e := range from.Extensions {
		fromExt[e.Name] = e
	}
	toExt := map[string]*model.Extension{}
	for _, e := range to.Extensions {
		toExt[e.Name] = e
	}
	for _, e := range to.Extensions {
		prev, ok := fromExt[e.Name]
		if !ok {
			cs.add(Op{Type: OpCreateExtension, Owner: extRef(e), Extension: e})
			continue
		}
		if prev.Version != "" && e.Version != "" && prev.Version != e.Version {
			cs.add(Op{Type: OpAlterExtension, Owner: extRef(e), Extension: e, PrevExt: prev})
		}
	}
	for _, e := range from.Extensions {
		if _, ok := toExt[e.Name]; !ok {
			cs.add(Op{Type: OpDropExtension, Owner: extRef(e), Extension: e})
		}
	}
}

func diffSchemas(cs *ChangeSet, from, to *model.Database) {
	fromS := indexSchemas(from)
	toS := indexSchemas(to)
	for name, s := range toS {
		if _, ok := fromS[name]; !ok {
			cs.add(Op{Type: OpCreateSchema, Owner: schemaRef(s), Schema: s})
		}
	}
	for name, s := range fromS {
		if _, ok := toS[name]; !ok {
			cs.add(Op{Type: OpDropSchema, Owner: schemaRef(s), Schema: s})
		}
	}
}

func diffEnums(cs *ChangeSet, from, to *model.Schema) {
	fromE := indexEnums(from)
	toE := indexEnums(to)
	for name, e := range toE {
		prev, ok := fromE[name]
		if !ok {
			cs.add(Op{Type: OpCreateEnum, Owner: e.Ref(), Enum: e})
			continue
		}
		if added := appendedValues(prev.Values, e.Values); len(added) > 0 {
			cs.add(Op{Type: OpAddEnumValue, Owner: e.Ref(), Enum: e, EnumValues: added})
		}
	}
	for name, e := range fromE {
		if _, ok := toE[name]; !ok {
			cs.add(Op{Type: OpDropEnum, Owner: e.Ref(), Enum: e})
		}
	}
}

func diffCollations(cs *ChangeSet, from, to *model.Schema) {
	fromC := indexCollations(from)
	toC := indexCollations(to)
	for name, c := range toC {
		prev, ok := fromC[name]
		if !ok {
			cs.add(Op{Type: OpCreateCollation, Owner: c.Ref(), Collation: c})
			continue
		}
		if prev.Definition != c.Definition {
			// Collations can't be altered in place; drop then recreate.
			cs.add(Op{Type: OpDropCollation, Owner: c.Ref(), Collation: prev})
			cs.add(Op{Type: OpCreateCollation, Owner: c.Ref(), Collation: c})
		}
	}
	for name, c := range fromC {
		if _, ok := toC[name]; !ok {
			cs.add(Op{Type: OpDropCollation, Owner: c.Ref(), Collation: c})
		}
	}
}

func diffSequences(cs *ChangeSet, from, to *model.Schema) {
	fromSeq := indexSequences(from)
	toSeq := indexSequences(to)
	for name, s := range toSeq {
		if s.IsOwned() {
			continue // created implicitly with its owning identity/serial column
		}
		prev, ok := fromSeq[name]
		if !ok {
			cs.add(Op{Type: OpCreateSequence, Owner: s.Ref(), Sequence: s})
			continue
		}
		if sequenceChanged(prev, s) {
			cs.add(Op{Type: OpAlterSequence, Owner: s.Ref(), Sequence: s, PrevSeq: prev})
		}
	}
	for name, s := range fromSeq {
		if s.IsOwned() {
			continue
		}
		if _, ok := toSeq[name]; !ok {
			cs.add(Op{Type: OpDropSequence, Owner: s.Ref(), Sequence: s})
		}
	}
}

func diffTables(cs *ChangeSet, from, to *model.Schema) {
	fromT := indexTables(from)
	toT := indexTables(to)
	for name, t := range toT {
		prev, ok := fromT[name]
		if !ok {
			// New table: emit the table (columns only) plus its constraints and
			// indexes as separate ops so the planner can order FKs after all
			// referenced tables exist.
			cs.add(Op{Type: OpCreateTable, Owner: t.Ref(), Table: t})
			for _, con := range t.Constraints {
				cs.add(Op{Type: OpAddConstraint, Owner: t.Ref(), Sub: con.Name, Table: t, Constraint: con})
			}
			for _, idx := range t.Indexes {
				cs.add(Op{Type: OpCreateIndex, Owner: t.Ref(), Sub: idx.Name, Table: t, Index: idx})
			}
			if t.RLSEnabled {
				cs.add(Op{Type: OpEnableRLS, Owner: t.Ref(), Table: t})
			}
			for _, tr := range t.Triggers {
				cs.add(Op{Type: OpCreateTrigger, Owner: t.Ref(), Sub: tr.Name, Table: t, Trigger: tr})
			}
			for _, pol := range t.Policies {
				cs.add(Op{Type: OpCreatePolicy, Owner: t.Ref(), Sub: pol.Name, Table: t, Policy: pol})
			}
			continue
		}
		diffTableColumns(cs, prev, t)
		diffTableConstraints(cs, prev, t)
		diffTableIndexes(cs, prev, t)
		diffTableTriggers(cs, prev, t)
		diffTablePolicies(cs, prev, t)
		diffTableRLS(cs, prev, t)
	}
	for name, t := range fromT {
		if _, ok := toT[name]; !ok {
			cs.add(Op{Type: OpDropTable, Owner: t.Ref(), Table: t})
		}
	}
}

func diffTableColumns(cs *ChangeSet, from, to *model.Table) {
	fromC := indexColumns(from)
	toC := indexColumns(to)
	for _, c := range to.Columns { // preserve target column order for adds
		prev, ok := fromC[c.Name]
		if !ok {
			cs.add(Op{Type: OpAddColumn, Owner: to.Ref(), Sub: c.Name, Table: to, Column: c})
			continue
		}
		diffColumn(cs, to, prev, c)
	}
	for _, c := range from.Columns {
		if _, ok := toC[c.Name]; !ok {
			cs.add(Op{Type: OpDropColumn, Owner: to.Ref(), Sub: c.Name, Table: to, Column: c})
		}
	}
}

func diffColumn(cs *ChangeSet, t *model.Table, prev, cur *model.Column) {
	if prev.Type != cur.Type || prev.Collation != cur.Collation {
		cs.add(Op{Type: OpAlterColumnType, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
	}
	if prev.Identity != cur.Identity {
		// Identity owns the column's value generation; the single identity op also
		// reconciles any default (e.g. a serial -> identity conversion), so the
		// plain default diff below is skipped while identity changes.
		cs.add(Op{Type: OpAlterColumnIdentity, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
	} else if prev.Default != cur.Default {
		if cur.Default == "" {
			cs.add(Op{Type: OpDropColumnDefault, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
		} else {
			cs.add(Op{Type: OpSetColumnDefault, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
		}
	}
	if prev.NotNull != cur.NotNull {
		if cur.NotNull {
			cs.add(Op{Type: OpSetColumnNotNull, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
		} else {
			cs.add(Op{Type: OpDropColumnNotNull, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
		}
	}
}

func diffTableConstraints(cs *ChangeSet, from, to *model.Table) {
	fromCon := indexConstraints(from)
	toCon := indexConstraints(to)
	for name, con := range toCon {
		prev, ok := fromCon[name]
		if !ok {
			cs.add(Op{Type: OpAddConstraint, Owner: to.Ref(), Sub: name, Table: to, Constraint: con})
			continue
		}
		if prev.Definition != con.Definition {
			// Constraints are not alterable in place: drop then re-add.
			cs.add(Op{Type: OpDropConstraint, Owner: to.Ref(), Sub: name, Table: to, Constraint: prev})
			cs.add(Op{Type: OpAddConstraint, Owner: to.Ref(), Sub: name, Table: to, Constraint: con})
		}
	}
	for name, con := range fromCon {
		if _, ok := toCon[name]; !ok {
			cs.add(Op{Type: OpDropConstraint, Owner: to.Ref(), Sub: name, Table: to, Constraint: con})
		}
	}
}

func diffTableIndexes(cs *ChangeSet, from, to *model.Table) {
	fromIdx := indexIndexes(from)
	toIdx := indexIndexes(to)
	for name, idx := range toIdx {
		prev, ok := fromIdx[name]
		if !ok {
			cs.add(Op{Type: OpCreateIndex, Owner: to.Ref(), Sub: name, Table: to, Index: idx})
			continue
		}
		if prev.Definition != idx.Definition {
			cs.add(Op{Type: OpDropIndex, Owner: to.Ref(), Sub: name, Table: to, Index: prev})
			cs.add(Op{Type: OpCreateIndex, Owner: to.Ref(), Sub: name, Table: to, Index: idx})
		}
	}
	for name, idx := range fromIdx {
		if _, ok := toIdx[name]; !ok {
			cs.add(Op{Type: OpDropIndex, Owner: to.Ref(), Sub: name, Table: to, Index: idx})
		}
	}
}

func diffTableTriggers(cs *ChangeSet, from, to *model.Table) {
	fromT := indexTriggers(from)
	toT := indexTriggers(to)
	for name, tr := range toT {
		prev, ok := fromT[name]
		if !ok {
			cs.add(Op{Type: OpCreateTrigger, Owner: to.Ref(), Sub: name, Table: to, Trigger: tr})
			continue
		}
		if prev.Definition != tr.Definition {
			// Triggers are recreated on change (CREATE OR REPLACE TRIGGER is PG14+).
			cs.add(Op{Type: OpDropTrigger, Owner: to.Ref(), Sub: name, Table: to, Trigger: prev})
			cs.add(Op{Type: OpCreateTrigger, Owner: to.Ref(), Sub: name, Table: to, Trigger: tr})
		}
	}
	for name, tr := range fromT {
		if _, ok := toT[name]; !ok {
			cs.add(Op{Type: OpDropTrigger, Owner: to.Ref(), Sub: name, Table: to, Trigger: tr})
		}
	}
}

func diffTablePolicies(cs *ChangeSet, from, to *model.Table) {
	fromP := indexPolicies(from)
	toP := indexPolicies(to)
	for name, pol := range toP {
		prev, ok := fromP[name]
		if !ok {
			cs.add(Op{Type: OpCreatePolicy, Owner: to.Ref(), Sub: name, Table: to, Policy: pol})
			continue
		}
		if policyChanged(prev, pol) {
			cs.add(Op{Type: OpDropPolicy, Owner: to.Ref(), Sub: name, Table: to, Policy: prev})
			cs.add(Op{Type: OpCreatePolicy, Owner: to.Ref(), Sub: name, Table: to, Policy: pol})
		}
	}
	for name, pol := range fromP {
		if _, ok := toP[name]; !ok {
			cs.add(Op{Type: OpDropPolicy, Owner: to.Ref(), Sub: name, Table: to, Policy: pol})
		}
	}
}

func diffTableRLS(cs *ChangeSet, from, to *model.Table) {
	if from.RLSEnabled != to.RLSEnabled || from.RLSForced != to.RLSForced {
		if to.RLSEnabled {
			cs.add(Op{Type: OpEnableRLS, Owner: to.Ref(), Table: to})
		} else {
			cs.add(Op{Type: OpDisableRLS, Owner: to.Ref(), Table: to})
		}
	}
}

func diffViews(cs *ChangeSet, from, to *model.Schema) {
	fromV := indexViews(from)
	toV := indexViews(to)
	for name, v := range toV {
		prev, ok := fromV[name]
		if !ok || prev.Definition != v.Definition || prev.CheckOption != v.CheckOption {
			// CREATE OR REPLACE handles both create and compatible body changes.
			cs.add(Op{Type: OpCreateView, Owner: v.Ref(), View: v})
		}
	}
	for name, v := range fromV {
		if _, ok := toV[name]; !ok {
			cs.add(Op{Type: OpDropView, Owner: v.Ref(), View: v})
		}
	}
}

func diffMatViews(cs *ChangeSet, from, to *model.Schema) {
	fromM := indexMatViews(from)
	toM := indexMatViews(to)
	for name, m := range toM {
		prev, ok := fromM[name]
		if !ok {
			cs.add(Op{Type: OpCreateMatView, Owner: m.Ref(), MatView: m})
			continue
		}
		if prev.Definition != m.Definition {
			// Materialized views cannot be replaced in place.
			cs.add(Op{Type: OpDropMatView, Owner: m.Ref(), MatView: prev})
			cs.add(Op{Type: OpCreateMatView, Owner: m.Ref(), MatView: m})
		}
	}
	for name, m := range fromM {
		if _, ok := toM[name]; !ok {
			cs.add(Op{Type: OpDropMatView, Owner: m.Ref(), MatView: m})
		}
	}
}
