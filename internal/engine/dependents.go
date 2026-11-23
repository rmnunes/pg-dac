package engine

import (
	"fmt"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// expandDependents makes blocking operations applicable by recreating the
// objects that currently depend on what they change. PostgreSQL refuses to
//
//   - change the type of, or drop, a column a view reads;
//   - drop a table, view or function another view still references;
//   - drop an object a function depends on: a relation whose row type its
//     signature uses, an enum in its signature, a relation or function a
//     SQL-standard body references;
//   - drop a function a trigger executes;
//   - drop a function a column default, a CHECK constraint, an index
//     expression or predicate, or a policy calls;
//
// and the differ deliberately emits those changes as plain ops. This pass walks
// the "from" model's dependency data (View.ColumnDeps / DependsOn,
// Function.DependsOn, Trigger.DependsOn), finds every dependent — transitively
// — and adds a DROP of its current form plus a CREATE of its desired form (or
// of its current form, when the desired state does not mention it and it can
// still exist). A recreated view gets its triggers back and a recreated
// materialized view its indexes. A default, CHECK constraint, index or policy
// that calls a function dropped here is removed before the drop and put back
// after the function is created (exprusers.go). The planner then brackets the blocking op
// with those drops and creates via the usual bands and edges. Dependents that
// reference an object that disappears for good are dropped only. It runs
// after the drop/include filters so it only reacts to changes that will really
// be applied, and its own drops are always paired with a create of the same
// object, so they are never filtered away.
func expandDependents(cs *diff.ChangeSet, from, to *model.Database) *diff.ChangeSet {
	if from == nil || to == nil || cs.Empty() {
		return cs
	}
	x := &expansion{
		to:          to,
		idx:         indexDependents(from),
		objects:     map[string]*objectState{},
		blockedCols: map[string]string{},
		droppedCols: map[string]bool{},
		trigDropped: map[string]bool{},
		trigCreated: map[string]bool{},
		idxCreated:  map[string]bool{},
		queued:      map[string]bool{},
		droppedHere: map[string]bool{},
		exprOps:     newExprOps(),
	}

	// What the change set already does.
	for _, op := range cs.Ops {
		switch {
		case op.Type == diff.OpAlterColumnType:
			x.blockedCols[colKey(op.Owner, op.Sub)] = fmt.Sprintf("column %s of %s, whose type changes from %s to %s",
				model.QuoteIdent(op.Sub), op.Owner.String(), columnType(op.PrevColumn), columnType(op.Column))
		case op.Type == diff.OpDropColumn:
			ck := colKey(op.Owner, op.Sub)
			x.blockedCols[ck] = fmt.Sprintf("column %s of %s, which is dropped", model.QuoteIdent(op.Sub), op.Owner.String())
			x.droppedCols[ck] = true
		case op.Type.IsObjectDrop():
			x.object(op.Owner).dropped = true
		case op.Type.IsObjectCreate():
			x.object(op.Owner).created = true
		case op.Type == diff.OpDropTrigger:
			x.trigDropped[colKey(op.Owner, op.Sub)] = true
		case op.Type == diff.OpCreateTrigger:
			x.trigCreated[colKey(op.Owner, op.Sub)] = true
		case op.Type == diff.OpCreateIndex || op.Type == diff.OpReplaceIndex:
			x.idxCreated[colKey(op.Owner, op.Sub)] = true
		}
		x.exprOps.record(op)
	}
	// Every drop seeds the frontier; it grows as dependents are themselves dropped.
	for key, st := range x.objects {
		if st.dropped {
			x.push(key)
		}
	}

	// Column-level seeds: views reading a column whose type changes or that is dropped.
	for ck, cause := range x.blockedCols {
		for _, d := range x.idx.byColumn[ck] {
			x.recreateRelation(d, cause)
		}
	}
	// Object-level seeds and everything they cascade to.
	for len(x.frontier) > 0 {
		k := x.frontier[0]
		x.frontier = x.frontier[1:]
		cause := x.objectCause(k)
		for _, d := range x.idx.byObject[k] {
			x.recreateRelation(d, cause)
		}
		for _, f := range x.idx.fnByObject[k] {
			x.recreateFunction(f, cause)
		}
		for _, t := range x.idx.triggersByFn[k] {
			x.recreateTrigger(t, k)
		}
		for _, u := range x.idx.exprByFn[k] {
			x.recreateExprUser(u, k)
		}
	}

	if len(x.added) == 0 && len(x.exprOps.splitReplace) == 0 {
		return cs
	}
	// Sub-object drops on a relation this pass drops would fail: PostgreSQL
	// drops them with the relation. (A relation the differ drops already had
	// its redundant sub-object drops avoided there.)
	out := &diff.ChangeSet{Ops: make([]diff.Op, 0, len(cs.Ops)+len(x.added))}
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpDropIndex, diff.OpDropTrigger, diff.OpDropConstraint, diff.OpDropPolicy:
			if x.droppedHere[op.Owner.Key()] {
				continue
			}
		case diff.OpReplaceIndex:
			// The relation is recreated here, so its old index is gone with it: the
			// new definition is created afresh instead of swapped in.
			if x.droppedHere[op.Owner.Key()] {
				op = diff.Op{Type: diff.OpCreateIndex, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Index: op.Index,
					Reason: "created with its new definition because its relation is recreated"}
			}
			// The old definition calls a function dropped here: it has to go
			// before that drop and the new one can only be built after the
			// function is back, so there is no swap.
			if reason, ok := x.exprOps.splitReplace[colKey(op.Owner, op.Sub)]; ok {
				out.Ops = append(out.Ops, diff.Op{Type: diff.OpDropIndex, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Index: op.PrevIndex, Reason: reason})
				op = diff.Op{Type: diff.OpCreateIndex, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Index: op.Index, Reason: reason}
			}
		}
		out.Ops = append(out.Ops, op)
	}
	out.Ops = append(out.Ops, x.added...)
	return out
}

// objectState records what the change set does to one top-level object.
type objectState struct {
	ref     model.ObjRef
	dropped bool // a drop op exists (incl. drop+recreate pairs)
	created bool // a create op exists
}

// expansion is the working state of one expandDependents run.
type expansion struct {
	to  *model.Database
	idx dependentIndex

	objects     map[string]*objectState // object key -> what happens to it
	blockedCols map[string]string       // column key -> why it blocks its dependents, in words
	droppedCols map[string]bool
	trigDropped map[string]bool
	trigCreated map[string]bool
	idxCreated  map[string]bool

	// frontier holds object keys whose dependents must be (re)created.
	frontier []string
	queued   map[string]bool
	// droppedHere marks the relations this pass dropped (their sub-object
	// drops in the change set become redundant).
	droppedHere map[string]bool

	added []diff.Op

	// exprOps tracks defaults, CHECK constraints, indexes and policies the
	// change set already drops or creates (exprusers.go).
	exprOps *exprOps
}

func (x *expansion) object(ref model.ObjRef) *objectState {
	st, ok := x.objects[ref.Key()]
	if !ok {
		st = &objectState{ref: ref}
		x.objects[ref.Key()] = st
	}
	return st
}

// gone reports whether an object disappears for good (dropped, not recreated).
func (x *expansion) gone(key string) bool {
	st := x.objects[key]
	return st != nil && st.dropped && !st.created
}

func (x *expansion) push(key string) {
	if !x.queued[key] {
		x.queued[key] = true
		x.frontier = append(x.frontier, key)
	}
}

// objectCause words the object behind a key and what happens to it, for a
// dependent's reason.
func (x *expansion) objectCause(key string) string {
	st := x.objects[key]
	if st.created {
		return st.ref.String() + ", which is dropped and recreated"
	}
	return st.ref.String() + ", which is dropped"
}

// recreate is the one algorithm behind every dependent kind: record the
// dependent as dropped, queue its own dependents, add the drop of its current
// form, then add the create of its desired form — or of its current form when
// the desired state does not mention it and it can still exist (nothing it
// references disappears for good). It returns the definition the object comes
// back with, or nil when it is dropped only, together with the reason both
// ops carry.
func recreate[T any](x *expansion, ref model.ObjRef, current, desired *T, invalid bool, cause string,
	drop, create func(*T, string) diff.Op) (*T, string) {
	key := ref.Key()
	st := x.object(ref)
	if st.dropped {
		return nil, "" // the differ, or an earlier step, already drops (and maybe recreates) it
	}
	st.dropped = true
	x.droppedHere[key] = true
	x.push(key)

	var recreated *T
	note := ""
	switch {
	case st.created:
		recreated = desired // the differ already recreates it in its desired form
	case desired != nil:
		recreated = desired
	case !invalid:
		recreated = current
		note = " (kept with its current definition; it is not part of the desired state)"
	}
	reason := "dropped because it depends on " + cause
	if recreated != nil {
		reason = "recreated because it depends on " + cause
	}
	x.added = append(x.added, drop(current, reason))
	if recreated != nil && !st.created {
		x.added = append(x.added, create(recreated, reason+note))
		st.created = true
	}
	return recreated, reason
}

// recreateRelation drops a dependent view or materialized view and recreates
// it, with its triggers (a view) or indexes (a materialized view), which
// vanish with it.
func (x *expansion) recreateRelation(d dependentRel, cause string) {
	invalid := false
	for _, dep := range d.deps {
		if x.gone(dep.Key()) {
			invalid = true
		}
	}
	for _, c := range d.colDeps {
		if x.droppedCols[colRefKey(c)] {
			invalid = true
		}
	}
	switch {
	case d.view != nil:
		v, reason := recreate(x, d.ref, d.view, x.to.View(d.ref.Schema, d.ref.Name), invalid, cause,
			func(v *model.View, reason string) diff.Op {
				return diff.Op{Type: diff.OpDropView, Owner: d.ref, View: v, Reason: reason}
			},
			func(v *model.View, reason string) diff.Op {
				return diff.Op{Type: diff.OpCreateView, Owner: d.ref, View: v, Reason: reason}
			})
		if v == nil {
			return
		}
		for _, tr := range v.Triggers {
			x.addTrigger(d.ref, nil, tr, reason)
		}
	case d.mview != nil:
		mv, reason := recreate(x, d.ref, d.mview, x.to.MatView(d.ref.Schema, d.ref.Name), invalid, cause,
			func(mv *model.MaterializedView, reason string) diff.Op {
				return diff.Op{Type: diff.OpDropMatView, Owner: d.ref, MatView: mv, Reason: reason}
			},
			func(mv *model.MaterializedView, reason string) diff.Op {
				return diff.Op{Type: diff.OpCreateMatView, Owner: d.ref, MatView: mv, Reason: reason}
			})
		if mv == nil {
			return
		}
		for _, ix := range mv.Indexes {
			x.addIndex(d.ref, ix, reason)
		}
	}
}

// recreateFunction drops a function that depends on an object dropped here
// (see Function.DependsOn) and recreates it. PostgreSQL refuses to drop a
// relation or type while such a function exists ("other objects depend on
// it").
func (x *expansion) recreateFunction(d dependentFn, cause string) {
	invalid := false
	for _, dep := range d.fn.DependsOn {
		if x.gone(dep.Key()) {
			invalid = true
		}
	}
	recreate(x, d.ref, d.fn, x.to.Function(d.ref.Schema, d.ref.Name), invalid, cause,
		func(f *model.Function, reason string) diff.Op {
			return diff.Op{Type: diff.OpDropFunction, Owner: d.ref, Function: f, Reason: reason}
		},
		func(f *model.Function, reason string) diff.Op {
			return diff.Op{Type: diff.OpCreateFunction, Owner: d.ref, Function: f, Reason: reason}
		})
}

// recreateTrigger drops a trigger that executes the function behind fnKey,
// which is dropped here, and adds it back in its desired form — or in its
// current one when the function comes back and the desired state does not
// mention the trigger. A trigger on a function gone for good is dropped only.
func (x *expansion) recreateTrigger(t dependentTrigger, fnKey string) {
	tk := colKey(t.rel, t.tr.Name)
	if rel := x.objects[t.rel.Key()]; x.trigDropped[tk] || rel != nil && rel.dropped {
		return // already handled, or the relation itself is dropped/recreated
	}
	x.trigDropped[tk] = true
	reason := "recreated because it executes " + x.objectCause(fnKey)
	x.added = append(x.added, diff.Op{Type: diff.OpDropTrigger, Owner: t.rel, Sub: t.tr.Name, Table: t.table, Trigger: t.tr, Reason: reason})
	switch desired, tbl := findTrigger(x.to, t.rel, t.tr.Name); {
	case desired != nil:
		x.addTrigger(t.rel, tbl, desired, reason)
	case !x.gone(fnKey):
		x.addTrigger(t.rel, t.table, t.tr, reason+" (kept with its current definition; it is not part of the desired state)")
	}
}

func (x *expansion) addTrigger(owner model.ObjRef, table *model.Table, tr *model.Trigger, reason string) {
	tk := colKey(owner, tr.Name)
	if x.trigCreated[tk] {
		return
	}
	x.trigCreated[tk] = true
	x.added = append(x.added, diff.Op{Type: diff.OpCreateTrigger, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr, Reason: reason})
	if tr.Enabled != "" {
		x.added = append(x.added, diff.Op{Type: diff.OpSetTriggerEnabled, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr})
	}
}

func (x *expansion) addIndex(owner model.ObjRef, ix *model.Index, reason string) {
	ik := colKey(owner, ix.Name)
	if x.idxCreated[ik] {
		return
	}
	x.idxCreated[ik] = true
	x.added = append(x.added, diff.Op{Type: diff.OpCreateIndex, Owner: owner, Sub: ix.Name, Index: ix, Reason: reason})
}

// dependentRel is a view or materialized view of the "from" model together with
// what it references.
type dependentRel struct {
	ref     model.ObjRef
	view    *model.View
	mview   *model.MaterializedView
	deps    []model.ObjRef
	colDeps []model.ColumnRef
}

// dependentTrigger is a trigger of the "from" model on a table or a view.
type dependentTrigger struct {
	rel   model.ObjRef
	table *model.Table // nil for a view trigger
	tr    *model.Trigger
}

// dependentFn is a function of the "from" model together with what it depends
// on (see Function.DependsOn).
type dependentFn struct {
	ref model.ObjRef
	fn  *model.Function
}

type dependentIndex struct {
	byObject     map[string][]dependentRel // relation/function/view key -> relations depending on it
	byColumn     map[string][]dependentRel // table key + "#" + column -> relations reading it
	fnByObject   map[string][]dependentFn  // object key -> functions depending on it
	triggersByFn map[string][]dependentTrigger
	exprByFn     map[string][]exprUser // function key -> defaults, CHECKs, indexes, policies calling it
}

func indexDependents(db *model.Database) dependentIndex {
	idx := dependentIndex{
		byObject:     map[string][]dependentRel{},
		byColumn:     map[string][]dependentRel{},
		fnByObject:   map[string][]dependentFn{},
		triggersByFn: map[string][]dependentTrigger{},
		exprByFn:     map[string][]exprUser{},
	}
	for _, s := range db.Schemas {
		for _, v := range s.Views {
			d := dependentRel{ref: v.Ref(), view: v, deps: v.DependsOn, colDeps: v.ColumnDeps}
			for _, dep := range v.DependsOn {
				idx.byObject[dep.Key()] = append(idx.byObject[dep.Key()], d)
			}
			for _, c := range v.ColumnDeps {
				idx.byColumn[colRefKey(c)] = append(idx.byColumn[colRefKey(c)], d)
			}
			for _, tr := range v.Triggers {
				for _, dep := range tr.DependsOn {
					idx.triggersByFn[dep.Key()] = append(idx.triggersByFn[dep.Key()], dependentTrigger{rel: v.Ref(), tr: tr})
				}
			}
		}
		for _, mv := range s.MatViews {
			d := dependentRel{ref: mv.Ref(), mview: mv, deps: mv.DependsOn, colDeps: mv.ColumnDeps}
			for _, dep := range mv.DependsOn {
				idx.byObject[dep.Key()] = append(idx.byObject[dep.Key()], d)
			}
			for _, c := range mv.ColumnDeps {
				idx.byColumn[colRefKey(c)] = append(idx.byColumn[colRefKey(c)], d)
			}
		}
		for _, mv := range s.MatViews {
			for _, ix := range mv.Indexes {
				idx.addExprUser(exprUser{owner: mv.Ref(), ix: ix}, ix.DependsOn)
			}
		}
		for _, t := range s.Tables {
			idx.indexExprUsers(t)
			for _, tr := range t.Triggers {
				for _, dep := range tr.DependsOn {
					idx.triggersByFn[dep.Key()] = append(idx.triggersByFn[dep.Key()], dependentTrigger{rel: t.Ref(), table: t, tr: tr})
				}
			}
		}
		for _, f := range s.Functions {
			d := dependentFn{ref: f.Ref(), fn: f}
			for _, dep := range f.DependsOn {
				idx.fnByObject[dep.Key()] = append(idx.fnByObject[dep.Key()], d)
			}
		}
	}
	return idx
}

// colKey identifies a column (or any relation sub-object) by its relation and
// name, matching the differ's Owner + Sub.
func colKey(rel model.ObjRef, sub string) string { return rel.Key() + "#" + sub }

func columnType(c *model.Column) string {
	if c == nil {
		return "?"
	}
	return c.Type
}

func colRefKey(c model.ColumnRef) string {
	return colKey(model.ObjRef{Kind: model.KindTable, Schema: c.Schema, Name: c.Table}, c.Column)
}

// findTrigger looks a trigger up on a table or a view of db; the table is nil
// for a view trigger.
func findTrigger(db *model.Database, rel model.ObjRef, name string) (*model.Trigger, *model.Table) {
	switch rel.Kind {
	case model.KindView:
		if v := db.View(rel.Schema, rel.Name); v != nil {
			for _, tr := range v.Triggers {
				if tr.Name == name {
					return tr, nil
				}
			}
		}
		return nil, nil
	default:
		t := db.Table(rel.Schema, rel.Name)
		if t == nil {
			return nil, nil
		}
		for _, tr := range t.Triggers {
			if tr.Name == name {
				return tr, t
			}
		}
		return nil, nil
	}
}
