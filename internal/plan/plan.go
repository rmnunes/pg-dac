// Package plan turns an unordered diff.ChangeSet into a dependency-correct,
// deterministic ordering of operations, reimplementing the approach of
// PostgreSQL's pg_dump_sort.c: a coarse object-type priority (modeled on
// pg_dump's dbObjectTypePriority and its PRE_DATA -> DATA -> POST_DATA sections)
// used purely as a tie-breaker on top of a real topological sort over derived
// dependency edges.
//
// Global strategy: all DROP operations sort before all CREATE/ALTER operations
// (drops in dependents-first order, creates in dependencies-first order). This
// makes drop+recreate of a same-named object correct and places foreign keys in
// a later band than the tables they reference (pg_dump's POST_DATA idea).
package plan

import (
	"github.com/rodrigonunes/pg-dac/internal/diff"
	"github.com/rodrigonunes/pg-dac/internal/model"
)

// Plan is the ordered sequence of operations to apply.
type Plan struct {
	Ops []diff.Op
	// Cyclic lists operations that took part in a dependency cycle and could
	// not be ordered; they were appended in priority order. Non-empty means the
	// migration will probably fail to apply as generated.
	Cyclic []diff.Op
}

// Empty reports whether the plan has no operations.
func (p *Plan) Empty() bool { return len(p.Ops) == 0 }

// Build orders a change set into an executable plan.
func Build(cs *diff.ChangeSet) *Plan {
	ops := cs.Ops
	n := len(ops)
	priority := make([]int, n)
	ordinal := make([]int, n)
	for i, op := range ops {
		priority[i] = opPriority(op)
		ordinal[i] = i
	}

	edges := deriveEdges(ops)
	order, cyclic := toposort(n, edges, priority, ordinal)

	out := make([]diff.Op, 0, n)
	for _, i := range order {
		out = append(out, ops[i])
	}
	p := &Plan{Ops: out}
	for _, i := range cyclic {
		p.Cyclic = append(p.Cyclic, ops[i])
	}
	return p
}

// opPriority returns the coarse ordering band for an operation. Drops occupy
// 10..89 (lower = dropped earlier, i.e. dependents first); creates/alters occupy
// 100..199 (lower = created earlier, i.e. dependencies first).
func opPriority(op diff.Op) int {
	switch op.Type {
	// --- drops: dependents first ---
	case diff.OpDropView:
		return 10
	case diff.OpDropMatView:
		return 11
	case diff.OpDropTrigger:
		return 12
	case diff.OpDropPolicy:
		return 13
	case diff.OpDropConstraint:
		return 20
	case diff.OpDropIndex:
		return 21
	case diff.OpDropColumn:
		return 30
	case diff.OpDropTable:
		return 40
	case diff.OpDropFunction:
		return 55
	case diff.OpDropCollation:
		return 58
	case diff.OpDropDomain, diff.OpDropCompositeType:
		// Before enums (a domain/composite may be built on one), after functions.
		return 59
	case diff.OpDropSequence:
		return 50
	case diff.OpDropEnum:
		return 60
	case diff.OpDropSchema:
		return 70
	case diff.OpDropExtension:
		return 80

	// --- creates/alters: dependencies first ---
	case diff.OpCreateExtension:
		return 100
	case diff.OpAlterExtension:
		return 101
	case diff.OpCreateSchema:
		return 110
	case diff.OpCreateCollation:
		// Before tables/indexes, which may reference the collation.
		return 118
	case diff.OpCreateEnum:
		return 120
	case diff.OpAddEnumValue:
		return 121
	case diff.OpCreateDomain:
		return 122
	case diff.OpCreateCompositeType:
		return 123
	case diff.OpCreateFunction:
		// Functions are created before tables so they are available to column
		// defaults, generated columns, and CHECK expressions.
		return 125
	case diff.OpCreateSequence:
		return 130
	case diff.OpAlterSequence:
		return 131
	case diff.OpCreateTable:
		return 140
	case diff.OpSetTableLogged, diff.OpSetRelOptions:
		return 144
	case diff.OpAddColumn:
		return 145
	case diff.OpAlterColumnType, diff.OpAlterColumnGenerated:
		return 146
	case diff.OpSetColumnDefault, diff.OpDropColumnDefault, diff.OpAlterColumnIdentity:
		return 147
	case diff.OpSetColumnNotNull, diff.OpDropColumnNotNull,
		diff.OpSetColumnStorage, diff.OpSetColumnCompression, diff.OpSetColumnStats:
		return 148
	case diff.OpSetSequenceOwner:
		// OWNED BY needs both the sequence (130) and the owning table/column
		// (140/145) to exist.
		return 149
	case diff.OpAddConstraint:
		// Foreign keys go in a later band than other constraints so they land
		// after every referenced table and its primary/unique keys exist.
		if op.Constraint != nil && op.Constraint.Type == model.ConstraintForeignKey {
			return 160
		}
		return 150
	case diff.OpCreateIndex:
		return 155
	case diff.OpValidateConstraint:
		return 165
	case diff.OpEnableRLS, diff.OpDisableRLS:
		return 167
	case diff.OpCreateTrigger, diff.OpSetTriggerEnabled:
		return 168
	case diff.OpCreatePolicy:
		return 169
	case diff.OpCreateView:
		return 170
	case diff.OpCreateMatView:
		return 175
	case diff.OpSetComment:
		return 185
	case diff.OpUnsupported:
		return 189
	}
	return 190
}
