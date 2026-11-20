package diff

import "github.com/rmnunes/pg-dac/internal/model"

// Options tunes what counts as a difference. The zero value is the default
// comparison: routine bodies are compared modulo whitespace, comments count,
// and a column order the target does not have is reported but not acted on.
type Options struct {
	// CompareWhitespace makes routine (function/procedure) bodies compare as
	// exact text. By default, differences in whitespace and line endings outside
	// string literals are not changes: PostgreSQL stores bodies verbatim, so a
	// body deployed from a CRLF checkout or reformatted by an editor would
	// otherwise be redeployed on every compare.
	CompareWhitespace bool
	// IgnoreComments also disregards SQL comments inside routine bodies (SSDT's
	// IgnoreComments). Implies whitespace-insensitive comparison.
	IgnoreComments bool
	// ColumnOrder says what to do when a table exists on both sides but its
	// declared column order differs from the target's physical order, which
	// PostgreSQL cannot change in place: ColumnOrderWarn (the default when
	// empty) reports it, ColumnOrderIgnore stays silent, ColumnOrderRebuild
	// rebuilds the table SSDT-style (new table in the declared order, rows
	// copied, swap) — a heavy, opt-in operation.
	ColumnOrder string
}

// Column-order policies (Options.ColumnOrder).
const (
	ColumnOrderIgnore  = "ignore"
	ColumnOrderWarn    = "warn"
	ColumnOrderRebuild = "rebuild"
)

// DiffWith computes the operations required to transform the "from" database
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
func DiffWith(from, to *model.Database, opts Options) *ChangeSet {
	cs := &ChangeSet{}

	diffExtensions(cs, from, to)
	diffSchemas(cs, from, to)

	fromSchemas := indexSchemas(from)
	toSchemas := indexSchemas(to)

	// Per-schema object diffing. Iterate the sorted union of schema names.
	rebuilt := map[string]bool{}
	for _, name := range unionKeys(fromSchemas, toSchemas) {
		fs := fromSchemas[name]
		ts := toSchemas[name]
		diffEnums(cs, fs, ts)
		diffDomains(cs, fs, ts)
		diffComposites(cs, fs, ts)
		diffCollations(cs, fs, ts)
		diffSequences(cs, from, to, fs, ts)
		diffFunctions(cs, fs, ts, opts)
		diffTables(cs, from, to, fs, ts, opts, rebuilt)
		diffViews(cs, fs, ts)
		diffMatViews(cs, fs, ts)
	}
	reAddForeignKeys(cs, from, to, rebuilt)
	return cs
}

// comment emits a COMMENT ON change for an object (or one of its sub-objects)
// that exists on both sides and is not being recreated.
func comment(cs *ChangeSet, owner model.ObjRef, kind, sub, prev, cur string) {
	if prev != cur {
		cs.add(Op{Type: OpSetComment, Owner: owner, Sub: sub, CommentKind: kind, Comment: cur})
	}
}

func diffFunctions(cs *ChangeSet, from, to *model.Schema, opts Options) {
	fromF := indexFunctions(from)
	toF := indexFunctions(to)
	for _, f := range functionsOf(to) {
		prev, ok := fromF[f.Signature()]
		if ok && routineEqual(prev, f, opts) {
			comment(cs, f.Ref(), "", "", prev.Comment, f.Comment)
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
		prev, ok := fromS[s.Name]
		if !ok {
			cs.add(Op{Type: OpCreateSchema, Owner: schemaRef(s), Schema: s})
			continue
		}
		comment(cs, schemaRef(s), "", "", prev.Comment, s.Comment)
	}
	for _, s := range from.Schemas {
		if _, ok := toS[s.Name]; !ok {
			cs.add(Op{Type: OpDropSchema, Owner: schemaRef(s), Schema: s})
		}
	}
}

// diffEnums adds new labels in their desired position (ADD VALUE BEFORE/AFTER);
// PostgreSQL cannot remove or reorder labels, so those changes surface as an
// unsupported-change hazard instead of silently vanishing.
func diffEnums(cs *ChangeSet, from, to *model.Schema) {
	fromE := indexEnums(from)
	toE := indexEnums(to)
	for _, e := range enumsOf(to) {
		prev, ok := fromE[e.Name]
		if !ok {
			cs.add(Op{Type: OpCreateEnum, Owner: e.Ref(), Enum: e})
			continue
		}
		comment(cs, e.Ref(), "", "", prev.Comment, e.Comment)
		added, positions := enumAdditions(prev.Values, e.Values)
		if len(added) > 0 {
			cs.add(Op{Type: OpAddEnumValue, Owner: e.Ref(), Enum: e, EnumValues: added, EnumPositions: positions})
		}
		if removed := missingValues(prev.Values, e.Values); len(removed) > 0 {
			cs.add(Op{Type: OpUnsupported, Owner: e.Ref(), Enum: e,
				Reason: "enum " + e.QualifiedName() + ": value(s) " + quoteList(removed) + " exist in the target but not in the source; PostgreSQL cannot drop enum values in place — recreate the type (and the columns using it) by hand"})
		} else if !sameRelativeOrder(prev.Values, e.Values) {
			cs.add(Op{Type: OpUnsupported, Owner: e.Ref(), Enum: e,
				Reason: "enum " + e.QualifiedName() + ": the existing values are ordered differently in the source; PostgreSQL cannot reorder enum values in place"})
		}
	}
	for _, e := range enumsOf(from) {
		if _, ok := toE[e.Name]; !ok {
			cs.add(Op{Type: OpDropEnum, Owner: e.Ref(), Enum: e})
		}
	}
}

func diffDomains(cs *ChangeSet, from, to *model.Schema) {
	fromD := indexDomains(from)
	toD := indexDomains(to)
	for _, d := range domainsOf(to) {
		prev, ok := fromD[d.Name]
		if !ok {
			cs.add(Op{Type: OpCreateDomain, Owner: d.Ref(), Domain: d})
			continue
		}
		if prev.Definition != d.Definition {
			cs.add(Op{Type: OpUnsupported, Owner: d.Ref(), Domain: d,
				Reason: "domain " + d.QualifiedName() + " changed (" + prev.Definition + " → " + d.Definition + "); altering a domain in use is not automated — apply the matching ALTER DOMAIN by hand"})
			continue
		}
		comment(cs, d.Ref(), "", "", prev.Comment, d.Comment)
	}
	for _, d := range domainsOf(from) {
		if _, ok := toD[d.Name]; !ok {
			cs.add(Op{Type: OpDropDomain, Owner: d.Ref(), Domain: d})
		}
	}
}

func diffComposites(cs *ChangeSet, from, to *model.Schema) {
	fromC := indexComposites(from)
	toC := indexComposites(to)
	for _, c := range compositesOf(to) {
		prev, ok := fromC[c.Name]
		if !ok {
			cs.add(Op{Type: OpCreateCompositeType, Owner: c.Ref(), Composite: c})
			continue
		}
		if prev.Definition != c.Definition {
			cs.add(Op{Type: OpUnsupported, Owner: c.Ref(), Composite: c,
				Reason: "composite type " + c.QualifiedName() + " changed (" + prev.Definition + " → " + c.Definition + "); altering a composite type in use is not automated — apply the matching ALTER TYPE by hand"})
			continue
		}
		comment(cs, c.Ref(), "", "", prev.Comment, c.Comment)
	}
	for _, c := range compositesOf(from) {
		if _, ok := toC[c.Name]; !ok {
			cs.add(Op{Type: OpDropCompositeType, Owner: c.Ref(), Composite: c})
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
			continue
		}
		comment(cs, c.Ref(), "", "", prev.Comment, c.Comment)
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
			// PrevSeq names the identity column's implicit sequence this one
			// replaces; it only disappears with that column's DROP IDENTITY, so
			// the planner orders the CREATE after it.
			op := Op{Type: OpCreateSequence, Owner: s.Ref(), Sequence: s}
			if ok {
				op.PrevSeq = prev
			}
			cs.add(op)
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
		comment(cs, s.Ref(), "", "", prev.Comment, s.Comment)
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

// diffTables compares a schema's tables; rebuilt collects the keys of tables
// the column-order policy rebuilds, for the foreign-key pass that follows.
func diffTables(cs *ChangeSet, fromDB, toDB *model.Database, from, to *model.Schema, opts Options, rebuilt map[string]bool) {
	fromT := indexTables(from)
	toT := indexTables(to)
	for _, t := range tablesOf(to) {
		prev, ok := fromT[t.Name]
		if ok && t.PartitionOf != nil && prev.PartitionOf != nil && *prev.PartitionOf != *t.PartitionOf {
			// A partition's parent or bound cannot be altered: recreate it. Its
			// rows are lost, so this is gated like any other drop.
			reason := "partition parent/bound changed (" + prev.PartitionOf.Bound + " → " + t.PartitionOf.Bound + "); a partition cannot be re-bound in place"
			cs.add(Op{Type: OpDropTable, Owner: t.Ref(), Table: prev, Reason: reason})
			ok = false
		}
		if !ok {
			// New table: emit the table (columns only) plus its constraints and
			// indexes as separate ops so the planner can order FKs after all
			// referenced tables exist.
			cs.add(Op{Type: OpCreateTable, Owner: t.Ref(), Table: t})
			emitColumnAttrs(cs, t, nil)
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
				emitTrigger(cs, t.Ref(), t, tr)
			}
			for _, pol := range t.Policies {
				cs.add(Op{Type: OpCreatePolicy, Owner: t.Ref(), Sub: pol.Name, Table: t, Policy: pol})
			}
			continue
		}
		if order := columnOrderMismatch(prev, t); order != "" {
			switch opts.ColumnOrder {
			case ColumnOrderIgnore:
			case ColumnOrderRebuild:
				if reason := rebuildBlocker(prev, t); reason != "" {
					cs.add(Op{Type: OpUnsupported, Owner: t.Ref(), Table: t,
						Reason: "table " + t.QualifiedName() + ": " + order + "; " + reason})
					break
				}
				emitRebuild(cs, toDB, prev, t, order)
				rebuilt[t.Ref().Key()] = true
				continue
			default: // warn
				cs.Warnings = append(cs.Warnings, "table "+t.QualifiedName()+": "+order+
					"; PostgreSQL cannot reorder columns in place, so the order is left as is. "+
					"To make the target match, set compare.columnOrder: rebuild (the table is rebuilt); "+
					"to make the project match the target instead, update the project from it: pg-dac publish --from <project> --to <target>")
			}
		}
		comment(cs, t.Ref(), "", "", prev.Comment, t.Comment)
		if prev.Persistence != t.Persistence {
			cs.add(Op{Type: OpSetTableLogged, Owner: t.Ref(), Table: t, PrevTable: prev})
		}
		if prev.PartitionKey != t.PartitionKey {
			cs.add(Op{Type: OpUnsupported, Owner: t.Ref(), Table: t,
				Reason: "table " + t.QualifiedName() + ": partitioning changed (" + orNone(prev.PartitionKey) + " → " + orNone(t.PartitionKey) + "); a table cannot be (re)partitioned in place"})
		}
		if set, reset := relOptionsDiff(prev.RelOptions, t.RelOptions); len(set) > 0 || len(reset) > 0 {
			cs.add(Op{Type: OpSetRelOptions, Owner: t.Ref(), Table: t, RelOptions: set, RelOptionsReset: reset})
		}
		diffTableColumns(cs, fromDB, toDB, prev, t)
		diffTableConstraints(cs, prev, t)
		diffTableIndexes(cs, prev, t)
		diffTriggers(cs, t.Ref(), t, prev.Triggers, t.Triggers, false)
		diffTablePolicies(cs, prev, t)
		diffTableRLS(cs, prev, t)
	}
	for _, t := range tablesOf(from) {
		if _, ok := toT[t.Name]; !ok {
			cs.add(Op{Type: OpDropTable, Owner: t.Ref(), Table: t})
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// emitColumnAttrs emits the storage/compression/statistics ops for columns of a
// (new) table whose attributes differ from the type defaults; prev is nil for a
// new table, else the previous column set for attribute diffs.
func emitColumnAttrs(cs *ChangeSet, t *model.Table, prev map[string]*model.Column) {
	for _, c := range t.Columns {
		var p *model.Column
		if prev != nil {
			p = prev[c.Name]
		}
		pStorage, pComp := "", ""
		var pStats *int
		if p != nil {
			pStorage, pComp, pStats = p.Storage, p.Compression, p.StatsTarget
		}
		if pStorage != c.Storage {
			cs.add(Op{Type: OpSetColumnStorage, Owner: t.Ref(), Sub: c.Name, Table: t, Column: c, PrevColumn: p})
		}
		if pComp != c.Compression {
			cs.add(Op{Type: OpSetColumnCompression, Owner: t.Ref(), Sub: c.Name, Table: t, Column: c, PrevColumn: p})
		}
		if !sameStats(pStats, c.StatsTarget) {
			cs.add(Op{Type: OpSetColumnStats, Owner: t.Ref(), Sub: c.Name, Table: t, Column: c, PrevColumn: p})
		}
	}
}

func sameStats(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func diffTableColumns(cs *ChangeSet, fromDB, toDB *model.Database, from, to *model.Table) {
	fromC := indexColumns(from)
	toC := indexColumns(to)
	for _, c := range to.Columns { // preserve target column order for adds
		prev, ok := fromC[c.Name]
		if !ok {
			cs.add(Op{Type: OpAddColumn, Owner: to.Ref(), Sub: c.Name, Table: to, Column: c})
			if c.Storage != "" {
				cs.add(Op{Type: OpSetColumnStorage, Owner: to.Ref(), Sub: c.Name, Table: to, Column: c})
			}
			if c.Compression != "" {
				cs.add(Op{Type: OpSetColumnCompression, Owner: to.Ref(), Sub: c.Name, Table: to, Column: c})
			}
			if c.StatsTarget != nil {
				cs.add(Op{Type: OpSetColumnStats, Owner: to.Ref(), Sub: c.Name, Table: to, Column: c})
			}
			continue
		}
		diffColumn(cs, fromDB, toDB, to, prev, c)
	}
	for _, c := range from.Columns {
		if _, ok := toC[c.Name]; !ok {
			cs.add(Op{Type: OpDropColumn, Owner: to.Ref(), Sub: c.Name, Table: to, Column: c})
		}
	}
}

func diffColumn(cs *ChangeSet, fromDB, toDB *model.Database, t *model.Table, prev, cur *model.Column) {
	if prev.Type != cur.Type || prev.Collation != cur.Collation {
		cs.add(Op{Type: OpAlterColumnType, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
	}
	if prev.Generated != cur.Generated || prev.GeneratedExpr != cur.GeneratedExpr {
		// A generation expression cannot be edited before PG17 and a plain column
		// cannot become generated: sqlgen picks SET EXPRESSION or drop + re-add.
		cs.add(Op{Type: OpAlterColumnGenerated, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
	}
	if prev.Identity != cur.Identity {
		// Identity owns the column's value generation. Gaining identity also
		// drops any serial default (a column cannot have both), so the default
		// diff below is skipped; the new identity sequence has never seen the
		// rows and is seeded. Losing identity is a plain DROP IDENTITY, and a
		// default the desired column carries is set by its own op afterwards —
		// the sequence it draws from may only be creatable once the identity
		// sequence of the same name is gone.
		if cur.Identity != "" {
			reason := ""
			if prev.Default != "" {
				// A serial column in the target and an identity column in the
				// source is usually two truths for one table, not a change anyone
				// decided: say how to resolve it either way.
				reason = "the target generates this column with DEFAULT " + prev.Default + ", the source declares an identity column; " +
					"publishing converts the target (seeded past its current maximum); to adopt the target's definition into the project instead: pg-dac publish --from <project> --to <target>"
			}
			cs.add(Op{Type: OpAlterColumnIdentity, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev, SeedSequence: isIntegerType(cur.Type), Reason: reason})
		} else {
			cs.add(Op{Type: OpAlterColumnIdentity, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
			if cur.Default != "" && cur.Generated == "" {
				seed := isIntegerType(cur.Type) && newSequenceDefault(fromDB, toDB, cur.Default)
				cs.add(Op{Type: OpSetColumnDefault, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev, SeedSequence: seed})
			}
		}
	} else if prev.Default != cur.Default && cur.Generated == "" {
		if cur.Default == "" {
			cs.add(Op{Type: OpDropColumnDefault, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
		} else {
			seed := isIntegerType(cur.Type) && newSequenceDefault(fromDB, toDB, cur.Default)
			cs.add(Op{Type: OpSetColumnDefault, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev, SeedSequence: seed})
		}
	}
	if prev.NotNull != cur.NotNull {
		if cur.NotNull {
			cs.add(Op{Type: OpSetColumnNotNull, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
		} else {
			cs.add(Op{Type: OpDropColumnNotNull, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
		}
	}
	if prev.Storage != cur.Storage {
		cs.add(Op{Type: OpSetColumnStorage, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
	}
	if prev.Compression != cur.Compression {
		cs.add(Op{Type: OpSetColumnCompression, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
	}
	if !sameStats(prev.StatsTarget, cur.StatsTarget) {
		cs.add(Op{Type: OpSetColumnStats, Owner: t.Ref(), Sub: cur.Name, Table: t, Column: cur, PrevColumn: prev})
	}
	comment(cs, t.Ref(), "column", cur.Name, prev.Comment, cur.Comment)
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
		comment(cs, to.Ref(), "constraint", con.Name, prev.Comment, con.Comment)
	}
	for _, con := range from.Constraints {
		if _, ok := toCon[con.Name]; !ok {
			cs.add(Op{Type: OpDropConstraint, Owner: to.Ref(), Sub: con.Name, Table: to, Constraint: con})
		}
	}
}

func diffTableIndexes(cs *ChangeSet, from, to *model.Table) {
	diffIndexes(cs, to.Ref(), to, from.Indexes, to.Indexes)
}

// diffIndexes compares the stand-alone indexes of a relation (table or
// materialized view); owner is the relation's reference.
func diffIndexes(cs *ChangeSet, owner model.ObjRef, table *model.Table, fromIdx, toIdx []*model.Index) {
	prevByName := map[string]*model.Index{}
	for _, i := range fromIdx {
		prevByName[i.Name] = i
	}
	curByName := map[string]*model.Index{}
	for _, i := range toIdx {
		curByName[i.Name] = i
	}
	// An index the target still has under a rebuild name and the source does
	// not know is a leftover of an interrupted rebuild: a rebuild may take its
	// name again, and it goes whatever the drop policy says.
	leftover := func(i *model.Index) bool { return isRebuildLeftover(i.Name) && curByName[i.Name] == nil }
	var fromKept []*model.Index
	for _, i := range fromIdx {
		if !leftover(i) {
			fromKept = append(fromKept, i)
		}
	}
	taken := indexNamesTaken(table, fromKept, toIdx)
	for _, idx := range toIdx {
		prev, ok := prevByName[idx.Name]
		if !ok {
			cs.add(Op{Type: OpCreateIndex, Owner: owner, Sub: idx.Name, Table: table, Index: idx})
			continue
		}
		if prev.Definition != idx.Definition {
			cs.add(Op{Type: OpReplaceIndex, Owner: owner, Sub: idx.Name, Table: table, Index: idx, PrevIndex: prev,
				TempIndexName: tempIndexName(idx.Name, taken)})
			continue
		}
		comment(cs, owner, "index", idx.Name, prev.Comment, idx.Comment)
	}
	for _, idx := range fromIdx {
		if _, ok := curByName[idx.Name]; ok {
			continue
		}
		if leftover(idx) {
			cs.add(Op{Type: OpDropIndex, Owner: owner, Sub: idx.Name, Table: table, Index: idx, Cleanup: true,
				Reason: "removed: an index left under its rebuild name by an interrupted earlier run"})
			continue
		}
		cs.add(Op{Type: OpDropIndex, Owner: owner, Sub: idx.Name, Table: table, Index: idx})
	}
}

// emitTrigger emits the CREATE TRIGGER (and its non-default enabled state) for
// a trigger on a new or recreated relation.
func emitTrigger(cs *ChangeSet, owner model.ObjRef, table *model.Table, tr *model.Trigger) {
	cs.add(Op{Type: OpCreateTrigger, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr})
	if tr.Enabled != "" {
		cs.add(Op{Type: OpSetTriggerEnabled, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr})
	}
}

// diffTriggers compares the triggers of a relation (table or view). When the
// relation is being recreated, its old triggers vanish with it and every desired
// trigger is created afresh.
func diffTriggers(cs *ChangeSet, owner model.ObjRef, table *model.Table, fromT, toT []*model.Trigger, recreated bool) {
	prevByName := map[string]*model.Trigger{}
	for _, t := range fromT {
		prevByName[t.Name] = t
	}
	curByName := map[string]*model.Trigger{}
	for _, t := range toT {
		curByName[t.Name] = t
	}
	for _, tr := range toT {
		prev, ok := prevByName[tr.Name]
		if !ok || recreated {
			emitTrigger(cs, owner, table, tr)
			continue
		}
		if prev.Definition != tr.Definition {
			// Triggers are recreated on change (CREATE OR REPLACE TRIGGER is PG14+).
			cs.add(Op{Type: OpDropTrigger, Owner: owner, Sub: tr.Name, Table: table, Trigger: prev})
			emitTrigger(cs, owner, table, tr)
			continue
		}
		if prev.Enabled != tr.Enabled {
			cs.add(Op{Type: OpSetTriggerEnabled, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr, PrevTrigger: prev})
		}
		comment(cs, owner, "trigger", tr.Name, prev.Comment, tr.Comment)
	}
	if recreated {
		return
	}
	for _, tr := range fromT {
		if _, ok := curByName[tr.Name]; !ok {
			cs.add(Op{Type: OpDropTrigger, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr})
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
			continue
		}
		comment(cs, to.Ref(), "policy", pol.Name, prev.Comment, pol.Comment)
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

func viewOptionsEqual(a, b *model.View) bool {
	return a.CheckOption == b.CheckOption && a.SecurityBarrier == b.SecurityBarrier && a.SecurityInvoker == b.SecurityInvoker
}

func diffViews(cs *ChangeSet, from, to *model.Schema) {
	fromV := indexViews(from)
	toV := indexViews(to)
	for _, v := range viewsOf(to) {
		prev, ok := fromV[v.Name]
		if !ok {
			cs.add(Op{Type: OpCreateView, Owner: v.Ref(), View: v})
			for _, tr := range v.Triggers {
				emitTrigger(cs, v.Ref(), nil, tr)
			}
			continue
		}
		if prev.Definition == v.Definition && viewOptionsEqual(prev, v) {
			comment(cs, v.Ref(), "", "", prev.Comment, v.Comment)
			diffTriggers(cs, v.Ref(), nil, prev.Triggers, v.Triggers, false)
			continue
		}
		if !viewReplaceCompatible(prev, v) {
			// CREATE OR REPLACE VIEW may only append columns; dependents are
			// recreated by the engine. The view's own triggers go with it.
			reason := "view columns were dropped, renamed, or retyped — CREATE OR REPLACE VIEW cannot do that, so the view is recreated"
			cs.add(Op{Type: OpDropView, Owner: v.Ref(), View: prev, Reason: reason})
			cs.add(Op{Type: OpCreateView, Owner: v.Ref(), View: v, Reason: reason})
			diffTriggers(cs, v.Ref(), nil, prev.Triggers, v.Triggers, true)
			continue
		}
		// CREATE OR REPLACE handles compatible body and option changes.
		cs.add(Op{Type: OpCreateView, Owner: v.Ref(), View: v})
		diffTriggers(cs, v.Ref(), nil, prev.Triggers, v.Triggers, false)
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
			for _, idx := range m.Indexes {
				cs.add(Op{Type: OpCreateIndex, Owner: m.Ref(), Sub: idx.Name, Index: idx})
			}
			continue
		}
		if prev.Definition != m.Definition {
			// Materialized views cannot be replaced in place; the indexes go
			// with the old one and are created afresh.
			cs.add(Op{Type: OpDropMatView, Owner: m.Ref(), MatView: prev})
			cs.add(Op{Type: OpCreateMatView, Owner: m.Ref(), MatView: m})
			for _, idx := range m.Indexes {
				cs.add(Op{Type: OpCreateIndex, Owner: m.Ref(), Sub: idx.Name, Index: idx})
			}
			continue
		}
		comment(cs, m.Ref(), "", "", prev.Comment, m.Comment)
		diffIndexes(cs, m.Ref(), nil, prev.Indexes, m.Indexes)
	}
	for _, m := range matViewsOf(from) {
		if _, ok := toM[m.Name]; !ok {
			cs.add(Op{Type: OpDropMatView, Owner: m.Ref(), MatView: m})
		}
	}
}
