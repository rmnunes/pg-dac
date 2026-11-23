package engine

import (
	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// exprUser is a part of a relation whose expression calls a user function: a
// column default, a CHECK constraint, an index (expression or predicate) or a
// policy. PostgreSQL refuses to drop the function while it exists, so when the
// function is dropped — for good, or to be recreated because CREATE OR REPLACE
// cannot change its result type — the part is removed first and put back
// once the function exists again.
type exprUser struct {
	owner model.ObjRef
	table *model.Table // nil for a materialized view's index
	col   *model.Column
	con   *model.Constraint
	ix    *model.Index
	pol   *model.Policy
}

func (u exprUser) name() string {
	switch {
	case u.col != nil:
		return u.col.Name
	case u.con != nil:
		return u.con.Name
	case u.ix != nil:
		return u.ix.Name
	default:
		return u.pol.Name
	}
}

func (idx dependentIndex) addExprUser(u exprUser, fns []model.ObjRef) {
	for _, fn := range fns {
		idx.exprByFn[fn.Key()] = append(idx.exprByFn[fn.Key()], u)
	}
}

// indexExprUsers files a table's defaults, CHECK constraints, indexes and
// policies under the functions they call.
func (idx dependentIndex) indexExprUsers(t *model.Table) {
	for _, c := range t.Columns {
		idx.addExprUser(exprUser{owner: t.Ref(), table: t, col: c}, c.DefaultDependsOn)
	}
	for _, con := range t.Constraints {
		idx.addExprUser(exprUser{owner: t.Ref(), table: t, con: con}, con.DependsOn)
	}
	for _, ix := range t.Indexes {
		idx.addExprUser(exprUser{owner: t.Ref(), table: t, ix: ix}, ix.DependsOn)
	}
	for _, pol := range t.Policies {
		idx.addExprUser(exprUser{owner: t.Ref(), table: t, pol: pol}, pol.DependsOn)
	}
}

// exprOps records which of those parts the change set already drops or
// creates, keyed by relation and name, so the expansion neither repeats the
// differ's work nor undoes it.
type exprOps struct {
	defaultDropped, defaultSet map[string]bool
	conDropped, conAdded       map[string]bool
	idxDropped                 map[string]bool
	idxReplaced                map[string]bool
	polDropped, polCreated     map[string]bool
	// splitReplace holds the index rebuilds that must become a drop before the
	// function drop and a create after it, with the reason.
	splitReplace map[string]string
}

func newExprOps() *exprOps {
	return &exprOps{
		defaultDropped: map[string]bool{}, defaultSet: map[string]bool{},
		conDropped: map[string]bool{}, conAdded: map[string]bool{},
		idxDropped: map[string]bool{}, idxReplaced: map[string]bool{},
		polDropped: map[string]bool{}, polCreated: map[string]bool{},
		splitReplace: map[string]string{},
	}
}

func (e *exprOps) record(op diff.Op) {
	k := colKey(op.Owner, op.Sub)
	switch op.Type {
	case diff.OpDropColumnDefault:
		e.defaultDropped[k] = true
	case diff.OpSetColumnDefault:
		e.defaultSet[k] = true
	case diff.OpDropConstraint:
		e.conDropped[k] = true
	case diff.OpAddConstraint:
		e.conAdded[k] = true
	case diff.OpDropIndex:
		e.idxDropped[k] = true
	case diff.OpReplaceIndex:
		e.idxReplaced[k] = true
	case diff.OpDropPolicy:
		e.polDropped[k] = true
	case diff.OpCreatePolicy:
		e.polCreated[k] = true
	}
}

// recreateExprUser removes a part that calls the function behind fnKey, which
// is dropped here, and puts it back: in its desired form, or in its current
// one when the desired state does not describe it and the function comes
// back. A part whose function is gone for good is only removed. When the
// change set already removes or replaces the part, nothing is added: the
// planner orders those ops around the function by the same references.
func (x *expansion) recreateExprUser(u exprUser, fnKey string) {
	if st := x.objects[u.owner.Key()]; st != nil && st.dropped {
		return // the relation is dropped (and maybe recreated) as a whole
	}
	k := colKey(u.owner, u.name())
	cause := x.objectCause(fnKey)
	gone := x.gone(fnKey)
	reason := "recreated because it calls " + cause
	if gone {
		reason = "dropped because it calls " + cause
	}
	const kept = " (kept with its current definition; it is not part of the desired state)"
	e := x.exprOps
	desiredTable := x.to.Table(u.owner.Schema, u.owner.Name)

	switch {
	case u.col != nil:
		if x.droppedCols[k] || e.defaultDropped[k] {
			return
		}
		e.defaultDropped[k] = true
		x.added = append(x.added, diff.Op{Type: diff.OpDropColumnDefault, Owner: u.owner, Sub: u.col.Name,
			Table: u.table, Column: u.col, PrevColumn: u.col, Reason: reason})
		if e.defaultSet[k] || gone {
			return // the differ sets the desired default after the function is back
		}
		table, col, note := u.table, u.col, kept
		if desiredTable != nil {
			if c := findColumn(desiredTable, u.col.Name); c != nil {
				table, col, note = desiredTable, c, ""
			}
		}
		if col.Default == "" {
			return
		}
		e.defaultSet[k] = true
		x.added = append(x.added, diff.Op{Type: diff.OpSetColumnDefault, Owner: u.owner, Sub: col.Name,
			Table: table, Column: col, PrevColumn: u.col, Reason: reason + note})

	case u.con != nil:
		if e.conDropped[k] {
			return
		}
		e.conDropped[k] = true
		x.added = append(x.added, diff.Op{Type: diff.OpDropConstraint, Owner: u.owner, Sub: u.con.Name,
			Table: u.table, Constraint: u.con, Reason: reason})
		if e.conAdded[k] || gone {
			return
		}
		table, con, note := u.table, u.con, kept
		if desiredTable != nil {
			if c := findConstraint(desiredTable, u.con.Name); c != nil {
				table, con, note = desiredTable, c, ""
			}
		}
		e.conAdded[k] = true
		x.added = append(x.added, diff.Op{Type: diff.OpAddConstraint, Owner: u.owner, Sub: con.Name,
			Table: table, Constraint: con, Reason: reason + note})

	case u.ix != nil:
		if e.idxDropped[k] {
			return
		}
		if e.idxReplaced[k] {
			e.splitReplace[k] = "rebuilt without a swap because it calls " + cause
			return
		}
		e.idxDropped[k] = true
		x.added = append(x.added, diff.Op{Type: diff.OpDropIndex, Owner: u.owner, Sub: u.ix.Name,
			Table: u.table, Index: u.ix, Reason: reason})
		if gone {
			return
		}
		ix, note := u.ix, kept
		if d := x.desiredIndex(u.owner, u.ix.Name); d != nil {
			ix, note = d, ""
		}
		x.addIndex(u.owner, ix, reason+note)

	case u.pol != nil:
		if e.polDropped[k] {
			return
		}
		e.polDropped[k] = true
		x.added = append(x.added, diff.Op{Type: diff.OpDropPolicy, Owner: u.owner, Sub: u.pol.Name,
			Table: u.table, Policy: u.pol, Reason: reason})
		if e.polCreated[k] || gone {
			return
		}
		table, pol, note := u.table, u.pol, kept
		if desiredTable != nil {
			if p := findPolicy(desiredTable, u.pol.Name); p != nil {
				table, pol, note = desiredTable, p, ""
			}
		}
		e.polCreated[k] = true
		x.added = append(x.added, diff.Op{Type: diff.OpCreatePolicy, Owner: u.owner, Sub: pol.Name,
			Table: table, Policy: pol, Reason: reason + note})
	}
}

// desiredIndex finds an index of a table or materialized view in the desired
// state.
func (x *expansion) desiredIndex(owner model.ObjRef, name string) *model.Index {
	var indexes []*model.Index
	if t := x.to.Table(owner.Schema, owner.Name); t != nil {
		indexes = t.Indexes
	} else if mv := x.to.MatView(owner.Schema, owner.Name); mv != nil {
		indexes = mv.Indexes
	}
	for _, ix := range indexes {
		if ix.Name == name {
			return ix
		}
	}
	return nil
}

func findColumn(t *model.Table, name string) *model.Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func findConstraint(t *model.Table, name string) *model.Constraint {
	for _, c := range t.Constraints {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func findPolicy(t *model.Table, name string) *model.Policy {
	for _, p := range t.Policies {
		if p.Name == name {
			return p
		}
	}
	return nil
}
