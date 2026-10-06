package engine

import (
	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// applyRebuildPolicy reconciles a table rebuild with the drop policy. The
// differ emits, for a rebuilt table, a drop of every sub-object (column,
// constraint, index, trigger, policy) the target has and the source lacks. A
// rebuild makes those vanish with the old table whatever the policy says, so:
//
//   - a drop the policy ALLOWS is removed (the old table takes it along; an
//     explicit drop afterwards would fail or be pointless) — a dropped column's
//     data is lost, which the rebuild statement reports as DELETES_DATA;
//   - a drop the policy FORBIDS becomes a carry-over: the column is appended to
//     the rebuilt table's definition (and therefore copied), the constraint,
//     index, trigger or policy is created again on the new table, and a serial
//     sequence owned by a kept column is detached and re-owned around the swap.
//
// It runs before filterDrops, which would otherwise discard the information.
func applyRebuildPolicy(cs *diff.ChangeSet, opts Options) *diff.ChangeSet {
	rebuilt := map[string]int{} // table key -> index of its rebuild create op
	for i, op := range cs.Ops {
		if op.Type == diff.OpCreateTable && op.Rebuild {
			rebuilt[op.Owner.Key()] = i
		}
	}
	if len(rebuilt) == 0 {
		return cs
	}

	type keep struct {
		cols []*model.Column
		seqs []*model.Sequence
	}
	kept := map[string]*keep{}
	keepFor := func(key string) *keep {
		k, ok := kept[key]
		if !ok {
			k = &keep{}
			kept[key] = k
		}
		return k
	}

	out := &diff.ChangeSet{Warnings: cs.Warnings}
	var carried []diff.Op
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpDropColumn, diff.OpDropConstraint, diff.OpDropIndex, diff.OpDropTrigger, diff.OpDropPolicy:
			if _, ok := rebuilt[op.Owner.Key()]; !ok {
				break
			}
			if dropAllowed(op, opts) {
				continue // goes with the old table
			}
			reason := "kept across the rebuild: the target has it, the source does not, and the drop policy does not drop it"
			switch op.Type {
			case diff.OpDropColumn:
				keepFor(op.Owner.Key()).cols = append(keepFor(op.Owner.Key()).cols, op.Column)
			case diff.OpDropConstraint:
				carried = append(carried, diff.Op{Type: diff.OpAddConstraint, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Constraint: op.Constraint, Reason: reason})
			case diff.OpDropIndex:
				carried = append(carried, diff.Op{Type: diff.OpCreateIndex, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Index: op.Index, Reason: reason})
			case diff.OpDropTrigger:
				carried = append(carried, diff.Op{Type: diff.OpCreateTrigger, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Trigger: op.Trigger, Reason: reason})
				if op.Trigger != nil && op.Trigger.Enabled != "" {
					carried = append(carried, diff.Op{Type: diff.OpSetTriggerEnabled, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Trigger: op.Trigger})
				}
			case diff.OpDropPolicy:
				carried = append(carried, diff.Op{Type: diff.OpCreatePolicy, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Policy: op.Policy, Reason: reason})
			}
			continue
		case diff.OpDropSequence:
			// A serial sequence owned by a column of the rebuilt table: dropped
			// explicitly when allowed (before the rebuild, while it exists), else
			// carried across the swap with its column.
			if op.Sequence != nil && op.Sequence.OwnedBy != nil {
				tbl := model.ObjRef{Kind: model.KindTable, Schema: op.Sequence.OwnedBy.Schema, Name: op.Sequence.OwnedBy.Table}
				if _, ok := rebuilt[tbl.Key()]; ok && !dropAllowed(op, opts) {
					keepFor(tbl.Key()).seqs = append(keepFor(tbl.Key()).seqs, op.Sequence)
					continue
				}
			}
		}
		out.Ops = append(out.Ops, op)
	}

	// Fold the carry-overs into the rebuild ops (indexes shifted: re-locate).
	for i := range out.Ops {
		op := &out.Ops[i]
		if op.Type != diff.OpCreateTable || !op.Rebuild {
			continue
		}
		k := kept[op.Owner.Key()]
		if k == nil {
			continue
		}
		if len(k.cols) > 0 {
			cp := *op.Table
			cp.Columns = append(append([]*model.Column{}, op.Table.Columns...), k.cols...)
			op.Table = &cp
		}
		op.OwnedSequences = append(append([]*model.Sequence{}, op.OwnedSequences...), k.seqs...)
	}
	out.Ops = append(out.Ops, carried...)
	return out
}
