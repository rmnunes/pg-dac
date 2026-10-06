package diff

import (
	"strings"

	"github.com/rmnunes/pg-dac/internal/model"
)

// columnOrderMismatch reports, as a human-readable sentence, how a table's
// declared column order differs from the target's when ALTERs cannot reach it,
// or "" when they can. PostgreSQL only ever appends columns, so after the
// adds and drops the differ emits, the target's order is "its surviving
// columns in their current order, then the new columns in declared order". The
// declared order is reachable exactly when it equals that.
func columnOrderMismatch(prev, cur *model.Table) string {
	have := map[string]bool{}
	for _, c := range cur.Columns {
		have[c.Name] = true
	}
	had := map[string]bool{}
	var reachable []string
	for _, c := range prev.Columns {
		had[c.Name] = true
		if have[c.Name] {
			reachable = append(reachable, c.Name)
		}
	}
	var declared []string
	for _, c := range cur.Columns {
		declared = append(declared, c.Name)
		if !had[c.Name] {
			reachable = append(reachable, c.Name)
		}
	}
	if strings.Join(reachable, "\x00") == strings.Join(declared, "\x00") {
		return ""
	}
	moved := misplaced(declared, reachable)
	noun := "column"
	if len(moved) > 1 {
		noun = "columns"
	}
	return noun + " " + strings.Join(moved, ", ") + " declared in a position the target does not have (the target appended " +
		map[bool]string{true: "them", false: "it"}[len(moved) > 1] + " later)"
}

// misplaced returns the smallest set of columns that would have to move for the
// target's order to become the declared one: the columns outside a longest
// common subsequence of the two orders (both are permutations of one set).
func misplaced(declared, reachable []string) []string {
	n, m := len(declared), len(reachable)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if declared[i] == reachable[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	kept := map[string]bool{}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case declared[i] == reachable[j]:
			kept[declared[i]] = true
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	var out []string
	for _, c := range declared {
		if !kept[c] {
			out = append(out, c)
		}
	}
	return out
}

// rebuildBlocker returns why a table cannot be rebuilt ("" when it can):
// partitioned tables and partitions carry structure a copy cannot reproduce.
func rebuildBlocker(prev, cur *model.Table) string {
	switch {
	case cur.PartitionKey != "" || prev.PartitionKey != "":
		return "a partitioned table cannot be rebuilt to change its column order"
	case cur.PartitionOf != nil || prev.PartitionOf != nil:
		return "a partition inherits its columns from its parent and cannot be rebuilt on its own"
	}
	return ""
}

// emitRebuild replaces the per-column diff of a table with a rebuild: a drop
// of the current table paired with a create of the desired one flagged as a
// rebuild (sqlgen renders the create as the atomic create-copy-swap block and
// the drop as nothing), followed by every desired sub-object as for a new
// table. Sub-objects the target has and the source lacks are emitted as drops,
// so the drop policy decides their fate like anywhere else — the engine then
// turns a drop the policy forbids into a carry-over (see engine.applyRebuildPolicy).
func emitRebuild(cs *ChangeSet, toDB *model.Database, prev, cur *model.Table, order string) {
	reason := "table rebuilt to reach the declared column order: " + order
	cs.add(Op{Type: OpDropTable, Owner: cur.Ref(), Table: prev, Reason: reason, Rebuild: true})
	cs.add(Op{Type: OpCreateTable, Owner: cur.Ref(), Table: cur, PrevTable: prev, Reason: reason, Rebuild: true,
		OwnedSequences: ownedSequences(toDB, cur)})
	emitColumnAttrs(cs, cur, nil)
	for _, con := range cur.Constraints {
		cs.add(Op{Type: OpAddConstraint, Owner: cur.Ref(), Sub: con.Name, Table: cur, Constraint: con})
	}
	for _, idx := range cur.Indexes {
		cs.add(Op{Type: OpCreateIndex, Owner: cur.Ref(), Sub: idx.Name, Table: cur, Index: idx})
	}
	if cur.RLSEnabled {
		cs.add(Op{Type: OpEnableRLS, Owner: cur.Ref(), Table: cur})
	}
	for _, tr := range cur.Triggers {
		emitTrigger(cs, cur.Ref(), cur, tr)
	}
	for _, pol := range cur.Policies {
		cs.add(Op{Type: OpCreatePolicy, Owner: cur.Ref(), Sub: pol.Name, Table: cur, Policy: pol})
	}

	// What the target has and the source does not: drops for the policy to judge.
	curCols := indexColumns(cur)
	for _, c := range prev.Columns {
		if _, ok := curCols[c.Name]; !ok {
			cs.add(Op{Type: OpDropColumn, Owner: cur.Ref(), Sub: c.Name, Table: cur, Column: c})
		}
	}
	curCons := indexConstraints(cur)
	for _, con := range prev.Constraints {
		if _, ok := curCons[con.Name]; !ok {
			cs.add(Op{Type: OpDropConstraint, Owner: cur.Ref(), Sub: con.Name, Table: cur, Constraint: con})
		}
	}
	curIdx := indexIndexes(cur)
	for _, idx := range prev.Indexes {
		if _, ok := curIdx[idx.Name]; !ok {
			cs.add(Op{Type: OpDropIndex, Owner: cur.Ref(), Sub: idx.Name, Table: cur, Index: idx})
		}
	}
	curTrig := indexTriggers(cur)
	for _, tr := range prev.Triggers {
		if _, ok := curTrig[tr.Name]; !ok {
			cs.add(Op{Type: OpDropTrigger, Owner: cur.Ref(), Sub: tr.Name, Table: cur, Trigger: tr})
		}
	}
	curPol := indexPolicies(cur)
	for _, pol := range prev.Policies {
		if _, ok := curPol[pol.Name]; !ok {
			cs.add(Op{Type: OpDropPolicy, Owner: cur.Ref(), Sub: pol.Name, Table: cur, Policy: pol})
		}
	}
}

// ownedSequences lists the explicit (serial-style) sequences of db owned by
// columns of t; identity sequences are implicit and recreated with the table.
func ownedSequences(db *model.Database, t *model.Table) []*model.Sequence {
	var out []*model.Sequence
	if db == nil {
		return nil
	}
	for _, s := range db.Schemas {
		for _, seq := range s.Sequences {
			if seq.OwnedBy != nil && seq.OwnedBy.Schema == t.Schema && seq.OwnedBy.Table == t.Name && !db.IsIdentitySequence(seq) {
				out = append(out, seq)
			}
		}
	}
	return out
}

// reAddForeignKeys drops, before a rebuild, and re-adds, after it, every
// foreign key of another table that references a rebuilt table — DROP TABLE
// would otherwise be refused. Only foreign keys that exist unchanged on both
// sides need this; a changed or new one is already emitted by the table diff.
func reAddForeignKeys(cs *ChangeSet, from, to *model.Database, rebuilt map[string]bool) {
	if len(rebuilt) == 0 {
		return
	}
	fromSchemas := indexSchemas(from)
	for _, s := range to.Schemas {
		fs := fromSchemas[s.Name]
		if fs == nil {
			continue
		}
		fromTables := indexTables(fs)
		for _, t := range s.Tables {
			if rebuilt[t.Ref().Key()] {
				continue // its own constraints are created afresh
			}
			prev := fromTables[t.Name]
			if prev == nil {
				continue
			}
			prevCons := indexConstraints(prev)
			for _, con := range t.Constraints {
				ref, ok := con.ForeignTableRef()
				if !ok || !rebuilt[ref.Key()] {
					continue
				}
				pc, ok := prevCons[con.Name]
				if !ok || pc.Definition != con.Definition {
					continue
				}
				reason := "re-added because " + ref.String() + " is rebuilt"
				cs.add(Op{Type: OpDropConstraint, Owner: t.Ref(), Sub: con.Name, Table: t, Constraint: pc, Reason: reason})
				cs.add(Op{Type: OpAddConstraint, Owner: t.Ref(), Sub: con.Name, Table: t, Constraint: con, Reason: reason})
			}
		}
	}
}
