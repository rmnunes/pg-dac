package diff

import "github.com/rodrigonunes/pg-dac/internal/model"

// Diff computes the operations required to transform the "from" database
// (current/target state) into the "to" database (desired state).
//
// Drops for objects present in "from" but absent from "to" are always emitted
// here; whether they are actually applied is governed by deploy options
// (DropObjectsNotInSource etc.) applied downstream, not by the differ.
//
// The walk is deterministic: every collection is visited in model order (the
// model sorts its collections by name; columns stay in attnum order) and never
// by ranging over a Go map. The planner uses discovery order as its tie-breaker
// within a priority band, so a non-deterministic walk here would make scripts —
// and the artifact's canonical SQL, hence its Merkle root — differ run to run.
func Diff(from, to *model.Database) *ChangeSet {
	cs := &ChangeSet{}

	diffExtensions(cs, from, to)
	diffSchemas(cs, from, to)

	fromSchemas := indexSchemas(from)
	toSchemas := indexSchemas(to)

	// Per-schema object diffing. Iterate the sorted union of schema names.
	for _, name := range unionKeys(fromSchemas, toSchemas) {
		fs := fromSchemas[name]
		ts := toSchemas[name]
		diffEnums(cs, fs, ts)
		diffCollations(cs, fs, ts)
		diffSequences(cs, from, to, fs, ts)
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
	for _, f := range functionsOf(to) {
		prev, ok := fromF[f.Signature()]
		if ok && prev.Definition == f.Definition {
			continue
		}
		if ok {
			if reason := functionRecreateReason(prev, f); reason != "" {
				// CREATE OR REPLACE cannot change the result type or kind: drop first.
				// Dependents (views, triggers) are recreated by the engine.
				cs.add(Op{Type: OpDropFunction, Owner: f.Ref(), Sub: f.IdentityArgs, Function: prev, Reason: reason})
				cs.add(Op{Type: OpCreateFunction, Owner: f.Ref(), Sub: f.IdentityArgs, Function: f, Reason: reason})
				continue
			}
		}
		// CREATE OR REPLACE covers both create and body change.
		cs.add(Op{Type: OpCreateFunction, Owner: f.Ref(), Sub: f.IdentityArgs, Function: f})
	}
	for _, f := range functionsOf(from) {
		if _, ok := toF[f.Signature()]; !ok {
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
	for _, s := range to.Schemas {
		if _, ok := fromS[s.Name]; !ok {
			cs.add(Op{Type: OpCreateSchema, Owner: schemaRef(s), Schema: s})
		}
	}
	for _, s := range from.Schemas {
		if _, ok := toS[s.Name]; !ok {
			cs.add(Op{Type: OpDropSchema, Owner: schemaRef(s), Schema: s})
		}
	}
}

func diffEnums(cs *ChangeSet, from, to *model.Schema) {
	fromE := indexEnums(from)
	toE := indexEnums(to)
	for _, e := range enumsOf(to) {
		prev, ok := fromE[e.Name]
		if !ok {
			cs.add(Op{Type: OpCreateEnum, Owner: e.Ref(), Enum: e})
			continue
		}
		if added := appendedValues(prev.Values, e.Values); len(added) > 0 {
			cs.add(Op{Type: OpAddEnumValue, Owner: e.Ref(), Enum: e, EnumValues: added})
		}
	}
	for _, e := range enumsOf(from) {
		if _, ok := toE[e.Name]; !ok {
			cs.add(Op{Type: OpDropEnum, Owner: e.Ref(), Enum: e})
		}
	}
}

func diffCollations(cs *ChangeSet, from, to *model.Schema) {
	fromC := indexCollations(from)
	toC := indexCollations(to)
	for _, c := range collationsOf(to) {
		prev, ok := fromC[c.Name]
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
	for _, c := range collationsOf(from) {
		if _, ok := toC[c.Name]; !ok {
			cs.add(Op{Type: OpDropCollation, Owner: c.Ref(), Collation: c})
		}
	}
}

// diffSequences handles standalone and serial-backed sequences. Identity
// columns' implicit sequences are skipped on both sides (PostgreSQL creates and
// drops them with the column). A serial-backed sequence is explicit: it must be
// created before the table whose DEFAULT calls nextval on it, and attached with
// OWNED BY once the table exists — so a new owned sequence yields two ops.
func diffSequences(cs *ChangeSet, fromDB, toDB *model.Database, from, to *model.Schema) {
	fromSeq := indexSequences(from)
	toSeq := indexSequences(to)
	for _, s := range sequencesOf(to) {
		if toDB.IsIdentitySequence(s) {
			continue // created implicitly with its GENERATED ... AS IDENTITY column
		}
		prev, ok := fromSeq[s.Name]
		if !ok || fromDB.IsIdentitySequence(prev) {
			cs.add(Op{Type: OpCreateSequence, Owner: s.Ref(), Sequence: s})
			if s.OwnedBy != nil {
				cs.add(Op{Type: OpSetSequenceOwner, Owner: s.Ref(), Sequence: s})
			}
			continue
		}
		if sequenceChanged(prev, s) {
			cs.add(Op{Type: OpAlterSequence, Owner: s.Ref(), Sequence: s, PrevSeq: prev})
		}
		if !sameOwner(prev.OwnedBy, s.OwnedBy) {
			cs.add(Op{Type: OpSetSequenceOwner, Owner: s.Ref(), Sequence: s, PrevSeq: prev})
		}
	}
	for _, s := range sequencesOf(from) {
		if fromDB.IsIdentitySequence(s) {
			continue
		}
		if cur, ok := toSeq[s.Name]; ok && !toDB.IsIdentitySequence(cur) {
			continue // present on both sides; handled above
		}
		// Gone from the desired state (or turned into an implicit identity
		// sequence there). If its owning column is itself going away, PostgreSQL
		// drops the owned sequence with the column — an explicit DROP would fail.
		if s.OwnedBy != nil && !toDB.HasColumn(*s.OwnedBy) {
			continue
		}
		cs.add(Op{Type: OpDropSequence, Owner: s.Ref(), Sequence: s})
	}
}

func diffTables(cs *ChangeSet, from, to *model.Schema) {
	fromT := indexTables(from)
	toT := indexTables(to)
	for _, t := range tablesOf(to) {
		prev, ok := fromT[t.Name]
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
	for _, t := range tablesOf(from) {
		if _, ok := toT[t.Name]; !ok {
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
	for _, con := range to.Constraints {
		prev, ok := fromCon[con.Name]
		if !ok {
			cs.add(Op{Type: OpAddConstraint, Owner: to.Ref(), Sub: con.Name, Table: to, Constraint: con})
			continue
		}
		if prev.Definition != con.Definition {
			// Constraints are not alterable in place: drop then re-add.
			cs.add(Op{Type: OpDropConstraint, Owner: to.Ref(), Sub: con.Name, Table: to, Constraint: prev})
			cs.add(Op{Type: OpAddConstraint, Owner: to.Ref(), Sub: con.Name, Table: to, Constraint: con})
			continue
		}
		if !prev.Validated && con.Validated {
			// Same body, but the target still holds it NOT VALID: validate in place.
			cs.add(Op{Type: OpValidateConstraint, Owner: to.Ref(), Sub: con.Name, Table: to, Constraint: con})
		}
	}
	for _, con := range from.Constraints {
		if _, ok := toCon[con.Name]; !ok {
			cs.add(Op{Type: OpDropConstraint, Owner: to.Ref(), Sub: con.Name, Table: to, Constraint: con})
		}
	}
}

func diffTableIndexes(cs *ChangeSet, from, to *model.Table) {
	fromIdx := indexIndexes(from)
	toIdx := indexIndexes(to)
	for _, idx := range to.Indexes {
		prev, ok := fromIdx[idx.Name]
		if !ok {
			cs.add(Op{Type: OpCreateIndex, Owner: to.Ref(), Sub: idx.Name, Table: to, Index: idx})
			continue
		}
		if prev.Definition != idx.Definition {
			cs.add(Op{Type: OpDropIndex, Owner: to.Ref(), Sub: idx.Name, Table: to, Index: prev})
			cs.add(Op{Type: OpCreateIndex, Owner: to.Ref(), Sub: idx.Name, Table: to, Index: idx})
		}
	}
	for _, idx := range from.Indexes {
		if _, ok := toIdx[idx.Name]; !ok {
			cs.add(Op{Type: OpDropIndex, Owner: to.Ref(), Sub: idx.Name, Table: to, Index: idx})
		}
	}
}

func diffTableTriggers(cs *ChangeSet, from, to *model.Table) {
	fromT := indexTriggers(from)
	toT := indexTriggers(to)
	for _, tr := range to.Triggers {
		prev, ok := fromT[tr.Name]
		if !ok {
			cs.add(Op{Type: OpCreateTrigger, Owner: to.Ref(), Sub: tr.Name, Table: to, Trigger: tr})
			continue
		}
		if prev.Definition != tr.Definition {
			// Triggers are recreated on change (CREATE OR REPLACE TRIGGER is PG14+).
			cs.add(Op{Type: OpDropTrigger, Owner: to.Ref(), Sub: tr.Name, Table: to, Trigger: prev})
			cs.add(Op{Type: OpCreateTrigger, Owner: to.Ref(), Sub: tr.Name, Table: to, Trigger: tr})
		}
	}
	for _, tr := range from.Triggers {
		if _, ok := toT[tr.Name]; !ok {
			cs.add(Op{Type: OpDropTrigger, Owner: to.Ref(), Sub: tr.Name, Table: to, Trigger: tr})
		}
	}
}

func diffTablePolicies(cs *ChangeSet, from, to *model.Table) {
	fromP := indexPolicies(from)
	toP := indexPolicies(to)
	for _, pol := range to.Policies {
		prev, ok := fromP[pol.Name]
		if !ok {
			cs.add(Op{Type: OpCreatePolicy, Owner: to.Ref(), Sub: pol.Name, Table: to, Policy: pol})
			continue
		}
		if policyChanged(prev, pol) {
			cs.add(Op{Type: OpDropPolicy, Owner: to.Ref(), Sub: pol.Name, Table: to, Policy: prev})
			cs.add(Op{Type: OpCreatePolicy, Owner: to.Ref(), Sub: pol.Name, Table: to, Policy: pol})
		}
	}
	for _, pol := range from.Policies {
		if _, ok := toP[pol.Name]; !ok {
			cs.add(Op{Type: OpDropPolicy, Owner: to.Ref(), Sub: pol.Name, Table: to, Policy: pol})
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
	for _, v := range viewsOf(to) {
		prev, ok := fromV[v.Name]
		if ok && prev.Definition == v.Definition && prev.CheckOption == v.CheckOption {
			continue
		}
		if ok && !viewReplaceCompatible(prev, v) {
			// CREATE OR REPLACE VIEW may only append columns; dependents are
			// recreated by the engine.
			reason := "view columns were dropped, renamed, or retyped — CREATE OR REPLACE VIEW cannot do that, so the view is recreated"
			cs.add(Op{Type: OpDropView, Owner: v.Ref(), View: prev, Reason: reason})
			cs.add(Op{Type: OpCreateView, Owner: v.Ref(), View: v, Reason: reason})
			continue
		}
		// CREATE OR REPLACE handles both create and compatible body changes.
		cs.add(Op{Type: OpCreateView, Owner: v.Ref(), View: v})
	}
	for _, v := range viewsOf(from) {
		if _, ok := toV[v.Name]; !ok {
			cs.add(Op{Type: OpDropView, Owner: v.Ref(), View: v})
		}
	}
}

func diffMatViews(cs *ChangeSet, from, to *model.Schema) {
	fromM := indexMatViews(from)
	toM := indexMatViews(to)
	for _, m := range matViewsOf(to) {
		prev, ok := fromM[m.Name]
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
	for _, m := range matViewsOf(from) {
		if _, ok := toM[m.Name]; !ok {
			cs.add(Op{Type: OpDropMatView, Owner: m.Ref(), MatView: m})
		}
	}
}
