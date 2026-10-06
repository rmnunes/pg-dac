package plan

import (
	"github.com/rodrigonunes/pg-dac/internal/diff"
	"github.com/rodrigonunes/pg-dac/internal/model"
)

// deriveEdges builds the "must come before" edges between operations. Edges
// refine the coarse priority bands for cases where two operations share a band
// or where a real dependency must be respected within a band (view-on-view,
// foreign keys to tables created in the same migration, sub-objects of a newly
// created table). edges[a] = nodes that must come after a.
func deriveEdges(ops []diff.Op) map[int][]int {
	edges := map[int][]int{}

	// Index object-create ops by their object key, and PK/unique constraint adds
	// by the table they belong to, and drop ops by object key.
	createByObj := map[string]int{}
	dropByObj := map[string]int{}
	pkUniqueAddByTable := map[string][]int{}
	for i, op := range ops {
		switch op.Type {
		case diff.OpCreateTable, diff.OpCreateView, diff.OpCreateMatView,
			diff.OpCreateEnum, diff.OpCreateSequence, diff.OpCreateSchema, diff.OpCreateExtension:
			createByObj[op.Owner.Key()] = i
		case diff.OpDropTable, diff.OpDropView, diff.OpDropMatView,
			diff.OpDropEnum, diff.OpDropSequence, diff.OpDropSchema, diff.OpDropExtension:
			dropByObj[op.Owner.Key()] = i
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

	for i, op := range ops {
		switch op.Type {
		case diff.OpAddColumn, diff.OpAlterColumnType, diff.OpSetColumnDefault,
			diff.OpDropColumnDefault, diff.OpSetColumnNotNull, diff.OpDropColumnNotNull,
			diff.OpCreateIndex, diff.OpCreateTrigger, diff.OpCreatePolicy, diff.OpEnableRLS:
			// Sub-object operations must follow their owning table's creation.
			if c, ok := createByObj[op.Owner.Key()]; ok {
				add(c, i)
			}

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

		case diff.OpCreateView, diff.OpCreateMatView:
			// Views/matviews must follow creation of everything they reference.
			for _, dep := range dependsOn(op) {
				if c, ok := createByObj[dep.Key()]; ok {
					add(c, i)
				}
			}

		case diff.OpDropView, diff.OpDropMatView:
			// A dropped view must be dropped before any object it depends on that
			// is also being dropped (dependents first).
			for _, dep := range dependsOn(op) {
				if d, ok := dropByObj[dep.Key()]; ok {
					add(i, d)
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
		}
	}
	return edges
}

// dependsOn returns the dependency references carried by a view/matview op.
func dependsOn(op diff.Op) []model.ObjRef {
	switch {
	case op.View != nil:
		return op.View.DependsOn
	case op.MatView != nil:
		return op.MatView.DependsOn
	}
	return nil
}
