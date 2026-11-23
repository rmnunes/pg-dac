package plan

import (
	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// deriveEdges builds the "must come before" edges between operations. Edges
// refine the coarse priority bands for cases where two operations share a band
// or where a real dependency must be respected across bands: view-on-view,
// foreign keys to tables created in the same migration, sub-objects of a newly
// created table, a function whose signature uses a row type created later, a
// trigger or view that calls such a function, a default, CHECK constraint,
// index or policy that calls a function dropped or created here, and a serial
// sequence's OWNED BY that needs its table. edges[a] = nodes that must come
// after a.
func deriveEdges(ops []diff.Op) map[int][]int {
	edges := map[int][]int{}

	// Index object creates/drops by object key, PK/unique constraint adds by
	// table, and column-level ops by (table, column).
	createByObj := map[string]int{}
	dropByObj := map[string]int{}
	pkUniqueAddByTable := map[string][]int{}
	addColumnByCol := map[string]int{}
	defaultOpsByCol := map[string][]int{}
	identityOpByCol := map[string]int{}  // ALTER COLUMN ... ADD/DROP IDENTITY
	colChangeByCol := map[string][]int{} // ALTER TYPE / DROP COLUMN, which dependent views must bracket
	trigCreateByKey := map[string]int{}  // relation key + "#" + trigger -> CreateTrigger op
	colKey := func(table model.ObjRef, col string) string { return table.Key() + "#" + col }
	colRefKey := func(c model.ColumnRef) string {
		return model.ObjRef{Kind: model.KindTable, Schema: c.Schema, Name: c.Table}.Key() + "#" + c.Column
	}

	for i, op := range ops {
		switch {
		case op.Type.IsObjectCreate():
			createByObj[op.Owner.Key()] = i
		case op.Type.IsObjectDrop():
			dropByObj[op.Owner.Key()] = i
		}
		switch op.Type {
		case diff.OpCreateTrigger:
			trigCreateByKey[colKey(op.Owner, op.Sub)] = i
		case diff.OpAddColumn:
			addColumnByCol[colKey(op.Owner, op.Sub)] = i
		case diff.OpSetColumnDefault, diff.OpDropColumnDefault, diff.OpAlterColumnIdentity:
			k := colKey(op.Owner, op.Sub)
			defaultOpsByCol[k] = append(defaultOpsByCol[k], i)
			if op.Type == diff.OpAlterColumnIdentity {
				identityOpByCol[k] = i
			}
		case diff.OpAlterColumnType, diff.OpDropColumn:
			k := colKey(op.Owner, op.Sub)
			colChangeByCol[k] = append(colChangeByCol[k], i)
		}
		if op.Type == diff.OpAddConstraint && op.Constraint != nil &&
			(op.Constraint.Type == model.ConstraintPrimaryKey || op.Constraint.Type == model.ConstraintUnique) {
			k := op.Owner.Key()
			pkUniqueAddByTable[k] = append(pkUniqueAddByTable[k], i)
		}
	}

	add := func(a, b int) {
		if a != b {
			edges[a] = append(edges[a], b)
		}
	}
	// An object dropped and created again (a function whose result type
	// changes, a view recreated around a column change) is dropped first. The
	// priority bands usually give that order, but an edge that holds the drop
	// back — a default or index that has to go before it — must not let the
	// create overtake it.
	for k, d := range dropByObj {
		if c, ok := createByObj[k]; ok {
			add(d, c)
		}
	}
	// afterCreates adds "create dep -> i" for every dependency being created here.
	afterCreates := func(i int, deps []model.ObjRef) {
		for _, dep := range deps {
			if c, ok := createByObj[dep.Key()]; ok {
				add(c, i)
			}
		}
	}
	// beforeDrops adds "i -> drop dep" so a dependent is dropped before its dependency.
	beforeDrops := func(i int, deps []model.ObjRef) {
		for _, dep := range deps {
			if d, ok := dropByObj[dep.Key()]; ok {
				add(i, d)
			}
		}
	}

	for i, op := range ops {
		// A default, CHECK constraint, index or policy calling a function goes
		// before that function is dropped and comes after it is created.
		removed, created := exprFunctionDeps(op)
		beforeDrops(i, removed)
		afterCreates(i, created)

		switch op.Type {
		case diff.OpAddColumn, diff.OpAlterColumnType, diff.OpSetColumnDefault,
			diff.OpDropColumnDefault, diff.OpSetColumnNotNull, diff.OpDropColumnNotNull,
			diff.OpAlterColumnIdentity, diff.OpAlterColumnGenerated,
			diff.OpSetColumnStorage, diff.OpSetColumnCompression, diff.OpSetColumnStats,
			diff.OpSetTableLogged, diff.OpSetRelOptions, diff.OpSetComment,
			diff.OpCreateIndex, diff.OpReplaceIndex, diff.OpCreatePolicy, diff.OpEnableRLS,
			diff.OpValidateConstraint:
			// Sub-object operations must follow their owning object's creation.
			if c, ok := createByObj[op.Owner.Key()]; ok {
				add(c, i)
			}
			// A column's default is set after its identity is dropped (a column
			// cannot hold both), and the sequence that default draws from has to
			// exist by then — see OpCreateSequence below.
			if op.Type == diff.OpSetColumnDefault || op.Type == diff.OpDropColumnDefault {
				if id, ok := identityOpByCol[colKey(op.Owner, op.Sub)]; ok {
					add(id, i)
				}
			}

		case diff.OpGrantDefault, diff.OpRevokeDefault:
			// An entry IN SCHEMA needs its schema.
			if op.DefaultPriv != nil && op.DefaultPriv.Schema != "" {
				if c, ok := createByObj[model.ObjRef{Kind: model.KindSchema, Name: op.DefaultPriv.Schema}.Key()]; ok {
					add(c, i)
				}
			}

		case diff.OpSetTriggerEnabled:
			if c, ok := createByObj[op.Owner.Key()]; ok {
				add(c, i)
			}
			if c, ok := trigCreateByKey[colKey(op.Owner, op.Sub)]; ok {
				add(c, i)
			}

		case diff.OpGrant:
			// Privileges follow the object (and, for a column, the column).
			if c, ok := createByObj[op.Owner.Key()]; ok {
				add(c, i)
			}
			if op.Sub != "" {
				if ac, ok := addColumnByCol[colKey(op.Owner, op.Sub)]; ok {
					add(ac, i)
				}
			}

		case diff.OpCreateSequence:
			// A sequence that takes over from an identity column's implicit
			// sequence of the same name can only be created once DROP IDENTITY
			// has removed that one.
			if op.PrevSeq != nil && op.PrevSeq.OwnedBy != nil {
				tbl := model.ObjRef{Kind: model.KindTable, Schema: op.PrevSeq.OwnedBy.Schema, Name: op.PrevSeq.OwnedBy.Table}
				if id, ok := identityOpByCol[colKey(tbl, op.PrevSeq.OwnedBy.Column)]; ok {
					add(id, i)
				}
			}

		case diff.OpCreateTable:
			// A partition needs its parent first.
			if op.Table != nil {
				if parent, ok := op.Table.ParentRef(); ok {
					if c, ok := createByObj[parent.Key()]; ok {
						add(c, i)
					}
				}
			}

		case diff.OpDropTable:
			// Drop partitions before their parent (the parent drop cascades, and an
			// explicit drop afterwards would fail).
			if op.Table != nil {
				if parent, ok := op.Table.ParentRef(); ok {
					if d, ok := dropByObj[parent.Key()]; ok {
						add(i, d)
					}
				}
			}

		case diff.OpCreateTrigger:
			if c, ok := createByObj[op.Owner.Key()]; ok {
				add(c, i)
			}
			afterCreates(i, dependsOn(op)) // ...and after the function it executes

		case diff.OpDropTrigger:
			beforeDrops(i, dependsOn(op)) // before the function it executes is dropped

		case diff.OpAddConstraint:
			// Constraint must follow its own table's creation.
			if c, ok := createByObj[op.Owner.Key()]; ok {
				add(c, i)
			}
			if op.Constraint != nil && op.Constraint.Type == model.ConstraintForeignKey {
				ref := model.ObjRef{Kind: model.KindTable, Schema: op.Constraint.RefSchema, Name: op.Constraint.RefTable}
				// FK must follow the referenced table's creation...
				if c, ok := createByObj[ref.Key()]; ok {
					add(c, i)
				}
				// ...and the referenced table's PK/unique constraint adds.
				for _, pk := range pkUniqueAddByTable[ref.Key()] {
					add(pk, i)
				}
			}

		case diff.OpCreateView, diff.OpCreateMatView, diff.OpCreateFunction:
			// Must follow creation of everything referenced (relations, enums,
			// functions — including a function's return/argument row types)...
			afterCreates(i, dependsOn(op))
			// ...and any type change / add of a column it reads.
			for _, c := range columnDepsOf(op) {
				for _, cc := range colChangeByCol[colRefKey(c)] {
					add(cc, i)
				}
				if ac, ok := addColumnByCol[colRefKey(c)]; ok {
					add(ac, i)
				}
			}

		case diff.OpDropView, diff.OpDropMatView, diff.OpDropFunction:
			// Dependents first: drop before any dependency that is also dropped...
			beforeDrops(i, dependsOn(op))
			// ...and before a type change / drop of a column it reads.
			for _, c := range columnDepsOf(op) {
				for _, cc := range colChangeByCol[colRefKey(c)] {
					add(i, cc)
				}
			}

		case diff.OpDropConstraint:
			// A dropped foreign key must be dropped before the referenced table
			// is dropped.
			if op.Constraint != nil && op.Constraint.Type == model.ConstraintForeignKey {
				ref := model.ObjRef{Kind: model.KindTable, Schema: op.Constraint.RefSchema, Name: op.Constraint.RefTable}
				if d, ok := dropByObj[ref.Key()]; ok {
					add(i, d)
				}
			}

		case diff.OpSetSequenceOwner:
			// OWNED BY needs the sequence and the owning table/column to exist.
			if c, ok := createByObj[op.Owner.Key()]; ok {
				add(c, i)
			}
			if op.Sequence != nil && op.Sequence.OwnedBy != nil {
				tbl := model.ObjRef{Kind: model.KindTable, Schema: op.Sequence.OwnedBy.Schema, Name: op.Sequence.OwnedBy.Table}
				if c, ok := createByObj[tbl.Key()]; ok {
					add(c, i)
				}
				if ac, ok := addColumnByCol[colKey(tbl, op.Sequence.OwnedBy.Column)]; ok {
					add(ac, i)
				}
			}

		case diff.OpDropSequence:
			// A sequence still owned by a surviving column can only go once that
			// column no longer defaults to it (serial -> identity, default change):
			// run after the column's default/identity ops even though drops
			// normally precede alters.
			if op.Sequence != nil && op.Sequence.OwnedBy != nil {
				tbl := model.ObjRef{Kind: model.KindTable, Schema: op.Sequence.OwnedBy.Schema, Name: op.Sequence.OwnedBy.Table}
				for _, c := range defaultOpsByCol[colKey(tbl, op.Sequence.OwnedBy.Column)] {
					add(c, i)
				}
			}
		}
	}
	return edges
}

// columnDepsOf returns the column-level dependencies carried by a view/matview op.
func columnDepsOf(op diff.Op) []model.ColumnRef {
	switch {
	case op.View != nil:
		return op.View.ColumnDeps
	case op.MatView != nil:
		return op.MatView.ColumnDeps
	}
	return nil
}

// dependsOn returns the dependency references carried by an op's payload.
func dependsOn(op diff.Op) []model.ObjRef {
	switch {
	case op.View != nil:
		return op.View.DependsOn
	case op.MatView != nil:
		return op.MatView.DependsOn
	case op.Function != nil:
		return op.Function.DependsOn
	case op.Trigger != nil:
		return op.Trigger.DependsOn
	}
	return nil
}

// exprFunctionDeps returns the functions an op's expressions call: those of
// what it removes (it must run before they are dropped) and those of what it
// creates (it must run after they are created). Dropping or creating a table
// covers its columns' defaults, its CHECK constraints, indexes and policies,
// and dropping a materialized view its indexes.
func exprFunctionDeps(op diff.Op) (removed, created []model.ObjRef) {
	switch op.Type {
	case diff.OpDropColumnDefault:
		if op.PrevColumn != nil {
			removed = op.PrevColumn.DefaultDependsOn
		}
	case diff.OpSetColumnDefault, diff.OpAddColumn:
		if op.Column != nil {
			created = op.Column.DefaultDependsOn
		}
	case diff.OpDropColumn:
		if op.Column != nil {
			removed = op.Column.DefaultDependsOn
		}
	case diff.OpDropConstraint:
		if op.Constraint != nil {
			removed = op.Constraint.DependsOn
		}
	case diff.OpAddConstraint:
		if op.Constraint != nil {
			created = op.Constraint.DependsOn
		}
	case diff.OpDropIndex:
		if op.Index != nil {
			removed = op.Index.DependsOn
		}
	case diff.OpCreateIndex, diff.OpReplaceIndex:
		if op.Index != nil {
			created = op.Index.DependsOn
		}
	case diff.OpDropPolicy:
		if op.Policy != nil {
			removed = op.Policy.DependsOn
		}
	case diff.OpCreatePolicy:
		if op.Policy != nil {
			created = op.Policy.DependsOn
		}
	case diff.OpDropTable:
		removed = tableExprDeps(op.Table)
	case diff.OpCreateTable:
		created = tableExprDeps(op.Table)
	case diff.OpDropMatView:
		if op.MatView != nil {
			for _, ix := range op.MatView.Indexes {
				removed = append(removed, ix.DependsOn...)
			}
		}
	}
	return removed, created
}

func tableExprDeps(t *model.Table) []model.ObjRef {
	if t == nil {
		return nil
	}
	var deps []model.ObjRef
	for _, c := range t.Columns {
		deps = append(deps, c.DefaultDependsOn...)
	}
	for _, c := range t.Constraints {
		deps = append(deps, c.DependsOn...)
	}
	for _, ix := range t.Indexes {
		deps = append(deps, ix.DependsOn...)
	}
	for _, p := range t.Policies {
		deps = append(deps, p.DependsOn...)
	}
	return deps
}
