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
//   - drop a relation whose row type a function's signature uses (or that a
//     SQL-standard function body references);
//   - drop a function a trigger executes;
//
// and the differ deliberately emits those changes as plain ops. This pass walks
// the "from" model's dependency data (View.ColumnDeps / DependsOn,
// Trigger.DependsOn), finds every dependent — transitively — and adds a DROP of
// its current form plus a CREATE of its desired form (or of its current form,
// when the desired state does not mention it and it can still exist). A
// recreated view gets its triggers back and a recreated materialized view its
// indexes. The planner then brackets the blocking op with those drops and
// creates via the usual bands and edges. Dependents that reference an object
// that disappears for good are dropped only. It runs after the drop/include
// filters so it only reacts to changes that will really be applied, and its
// own drops are always paired with a create of the same object, so they are
// never filtered away.
func expandDependents(cs *diff.ChangeSet, from, to *model.Database) *diff.ChangeSet {
	if from == nil || to == nil || cs.Empty() {
		return cs
	}
	idx := indexDependents(from)

	// What the change set already does.
	dropped := map[string]bool{} // object keys with a drop op (incl. drop+recreate pairs)
	created := map[string]bool{} // object keys with a create op
	blockedCols := map[string]bool{}
	droppedCols := map[string]bool{}
	trigDropped := map[string]bool{}
	trigCreated := map[string]bool{}
	idxCreated := map[string]bool{}
	colCause := map[string]string{}     // why a column blocks its dependents, in words
	objRef := map[string]model.ObjRef{} // the object behind a dropped/created key, for the same
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpAlterColumnType:
			ck := colKey(op.Owner, op.Sub)
			blockedCols[ck] = true
			colCause[ck] = fmt.Sprintf("column %s of %s, whose type changes from %s to %s",
				model.QuoteIdent(op.Sub), op.Owner.String(), columnType(op.PrevColumn), columnType(op.Column))
		case diff.OpDropColumn:
			ck := colKey(op.Owner, op.Sub)
			blockedCols[ck] = true
			droppedCols[ck] = true
			colCause[ck] = fmt.Sprintf("column %s of %s, which is dropped", model.QuoteIdent(op.Sub), op.Owner.String())
		case diff.OpDropView, diff.OpDropMatView, diff.OpDropFunction, diff.OpDropTable:
			dropped[op.Owner.Key()] = true
			objRef[op.Owner.Key()] = op.Owner
		case diff.OpCreateView, diff.OpCreateMatView, diff.OpCreateFunction, diff.OpCreateTable:
			created[op.Owner.Key()] = true
			objRef[op.Owner.Key()] = op.Owner
		case diff.OpDropTrigger:
			trigDropped[colKey(op.Owner, op.Sub)] = true
		case diff.OpCreateTrigger:
			trigCreated[colKey(op.Owner, op.Sub)] = true
		case diff.OpCreateIndex, diff.OpReplaceIndex:
			idxCreated[colKey(op.Owner, op.Sub)] = true
		}
	}
	// gone reports whether an object disappears for good (dropped, not recreated).
	gone := func(key string) bool { return dropped[key] && !created[key] }

	var added []diff.Op
	// frontier holds object keys whose dependents must be (re)created; it is
	// seeded with every drop and grows as dependents are themselves dropped.
	var frontier []string
	queued := map[string]bool{}
	push := func(k string) {
		if !queued[k] {
			queued[k] = true
			frontier = append(frontier, k)
		}
	}
	for k := range dropped {
		push(k)
	}

	addTrigger := func(owner model.ObjRef, table *model.Table, tr *model.Trigger, reason string) {
		tk := colKey(owner, tr.Name)
		if trigCreated[tk] {
			return
		}
		trigCreated[tk] = true
		added = append(added, diff.Op{Type: diff.OpCreateTrigger, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr, Reason: reason})
		if tr.Enabled != "" {
			added = append(added, diff.Op{Type: diff.OpSetTriggerEnabled, Owner: owner, Sub: tr.Name, Table: table, Trigger: tr})
		}
	}
	addIndex := func(owner model.ObjRef, ix *model.Index, reason string) {
		ik := colKey(owner, ix.Name)
		if idxCreated[ik] {
			return
		}
		idxCreated[ik] = true
		added = append(added, diff.Op{Type: diff.OpCreateIndex, Owner: owner, Sub: ix.Name, Index: ix, Reason: reason})
	}

	// recreate drops a dependent relation and recreates it in its desired form.
	recreate := func(d dependentRel, cause string) {
		key := d.ref.Key()
		if dropped[key] {
			return // already dropped by the differ or an earlier step
		}
		dropped[key] = true
		objRef[key] = d.ref
		push(key)
		reason := fmt.Sprintf("recreated because it depends on %s", cause)

		// Can the current definition survive? Not if it references an object or
		// column that disappears for good.
		invalid := false
		for _, dep := range d.deps {
			if gone(dep.Key()) {
				invalid = true
			}
		}
		for _, c := range d.colDeps {
			if droppedCols[colRefKey(c)] {
				invalid = true
			}
		}

		switch {
		case d.view != nil:
			added = append(added, diff.Op{Type: diff.OpDropView, Owner: d.ref, View: d.view, Reason: reason})
			var recreated *model.View
			switch desired := findView(to, d.ref); {
			case created[key]:
				recreated = desired // the differ already recreates it in its desired form
			case desired != nil:
				added = append(added, diff.Op{Type: diff.OpCreateView, Owner: d.ref, View: desired, Reason: reason})
				created[key] = true
				recreated = desired
			case !invalid:
				added = append(added, diff.Op{Type: diff.OpCreateView, Owner: d.ref, View: d.view,
					Reason: reason + " (kept with its current definition; it is not part of the desired state)"})
				created[key] = true
				recreated = d.view
			}
			if recreated != nil {
				// The view's triggers vanished with it.
				for _, tr := range recreated.Triggers {
					addTrigger(d.ref, nil, tr, reason)
				}
			}
		case d.mview != nil:
			added = append(added, diff.Op{Type: diff.OpDropMatView, Owner: d.ref, MatView: d.mview, Reason: reason})
			var recreated *model.MaterializedView
			switch desired := findMatView(to, d.ref); {
			case created[key]:
				recreated = desired
			case desired != nil:
				added = append(added, diff.Op{Type: diff.OpCreateMatView, Owner: d.ref, MatView: desired, Reason: reason})
				created[key] = true
				recreated = desired
			case !invalid:
				added = append(added, diff.Op{Type: diff.OpCreateMatView, Owner: d.ref, MatView: d.mview,
					Reason: reason + " (kept with its current definition; it is not part of the desired state)"})
				created[key] = true
				recreated = d.mview
			}
			if recreated != nil {
				// The materialized view's indexes vanished with it.
				for _, ix := range recreated.Indexes {
					addIndex(d.ref, ix, reason)
				}
			}
		}
	}

	// recreateFn drops a function whose signature uses (or whose SQL-standard
	// body references) an object dropped here, and recreates it in its desired
	// form — or its current one when the desired state does not mention it and
	// it can still exist. PostgreSQL refuses to drop a view or table while such
	// a function exists ("other objects depend on it").
	recreateFn := func(d dependentFn, cause string) {
		key := d.ref.Key()
		if dropped[key] {
			return // the differ already drops (and maybe recreates) it
		}
		dropped[key] = true
		objRef[key] = d.ref
		push(key)
		reason := fmt.Sprintf("recreated because it depends on %s", cause)
		added = append(added, diff.Op{Type: diff.OpDropFunction, Owner: d.ref, Function: d.fn, Reason: reason})
		invalid := false
		for _, dep := range d.fn.DependsOn {
			if gone(dep.Key()) {
				invalid = true
			}
		}
		switch desired := findFunction(to, d.ref); {
		case created[key]:
			// the differ already creates its desired form
		case desired != nil:
			added = append(added, diff.Op{Type: diff.OpCreateFunction, Owner: d.ref, Function: desired, Reason: reason})
			created[key] = true
		case !invalid:
			added = append(added, diff.Op{Type: diff.OpCreateFunction, Owner: d.ref, Function: d.fn,
				Reason: reason + " (kept with its current definition; it is not part of the desired state)"})
			created[key] = true
		}
	}

	// objectCause words the object behind a key and what happens to it, for a
	// dependent's reason.
	objectCause := func(k string) string {
		ref, ok := objRef[k]
		if !ok {
			return k[len(prefixLen(k)):]
		}
		if created[k] {
			return ref.String() + ", which is dropped and recreated"
		}
		return ref.String() + ", which is dropped"
	}

	// Column-level seeds: views reading a column whose type changes or that is dropped.
	for ck := range blockedCols {
		for _, d := range idx.byColumn[ck] {
			recreate(d, colCause[ck])
		}
	}
	// Object-level seeds and everything they cascade to.
	for len(frontier) > 0 {
		k := frontier[0]
		frontier = frontier[1:]
		for _, d := range idx.byObject[k] {
			recreate(d, objectCause(k))
		}
		// Functions whose signature uses the dropped object's row type, or that
		// reference it from a SQL-standard body.
		for _, f := range idx.fnByObject[k] {
			recreateFn(f, objectCause(k))
		}
		// Triggers executing a dropped function.
		for _, t := range idx.triggersByFn[k] {
			tk := colKey(t.rel, t.tr.Name)
			if trigDropped[tk] || gone(t.rel.Key()) || dropped[t.rel.Key()] {
				continue // already handled, or the relation itself is dropped/recreated
			}
			trigDropped[tk] = true
			reason := "recreated because it executes " + objectCause(k)
			added = append(added, diff.Op{Type: diff.OpDropTrigger, Owner: t.rel, Sub: t.tr.Name, Table: t.table, Trigger: t.tr, Reason: reason})
			if desired, tbl := findTrigger(to, t.rel, t.tr.Name); desired != nil {
				addTrigger(t.rel, tbl, desired, reason)
			} else if created[k] {
				addTrigger(t.rel, t.table, t.tr, reason+" (kept with its current definition; it is not part of the desired state)")
			}
		}
	}

	if len(added) == 0 {
		return cs
	}
	// Sub-object drops on a relation that is itself dropped (and recreated)
	// here would fail: PostgreSQL drops them with the relation.
	out := &diff.ChangeSet{Ops: make([]diff.Op, 0, len(cs.Ops)+len(added))}
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpDropIndex, diff.OpDropTrigger, diff.OpDropConstraint, diff.OpDropPolicy:
			if dropped[op.Owner.Key()] && queued[op.Owner.Key()] && isRelationRecreatedHere(op.Owner, added) {
				continue
			}
		case diff.OpReplaceIndex:
			// The relation is recreated here, so its old index is gone with it: the
			// new definition is created afresh instead of swapped in.
			if dropped[op.Owner.Key()] && queued[op.Owner.Key()] && isRelationRecreatedHere(op.Owner, added) {
				op = diff.Op{Type: diff.OpCreateIndex, Owner: op.Owner, Sub: op.Sub, Table: op.Table, Index: op.Index,
					Reason: "the relation is recreated; the index is created with its new definition"}
			}
		}
		out.Ops = append(out.Ops, op)
	}
	out.Ops = append(out.Ops, added...)
	return out
}

// isRelationRecreatedHere reports whether this pass added the drop of the
// relation (as opposed to the differ having emitted it, in which case the
// differ already avoided redundant sub-object drops).
func isRelationRecreatedHere(rel model.ObjRef, added []diff.Op) bool {
	for _, op := range added {
		if (op.Type == diff.OpDropView || op.Type == diff.OpDropMatView) && op.Owner.Key() == rel.Key() {
			return true
		}
	}
	return false
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

// dependentFn is a function of the "from" model that depends on another object
// (a relation whose row type its signature uses, an enum, a SQL-standard body's
// references).
type dependentFn struct {
	ref model.ObjRef
	fn  *model.Function
}

type dependentIndex struct {
	byObject     map[string][]dependentRel // relation/function/view key -> relations depending on it
	byColumn     map[string][]dependentRel // table key + "#" + column -> relations reading it
	fnByObject   map[string][]dependentFn  // object key -> functions depending on it
	triggersByFn map[string][]dependentTrigger
}

func indexDependents(db *model.Database) dependentIndex {
	idx := dependentIndex{
		byObject:     map[string][]dependentRel{},
		byColumn:     map[string][]dependentRel{},
		fnByObject:   map[string][]dependentFn{},
		triggersByFn: map[string][]dependentTrigger{},
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
		for _, t := range s.Tables {
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

func findFunction(db *model.Database, ref model.ObjRef) *model.Function {
	for _, s := range db.Schemas {
		if s.Name != ref.Schema {
			continue
		}
		for _, f := range s.Functions {
			if f.Signature() == ref.Name {
				return f
			}
		}
	}
	return nil
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

// prefixLen returns the "kind:" prefix of a key so the human readable cause
// can show the qualified name only.
func prefixLen(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i+1]
		}
	}
	return ""
}

func findView(db *model.Database, ref model.ObjRef) *model.View {
	for _, s := range db.Schemas {
		if s.Name != ref.Schema {
			continue
		}
		for _, v := range s.Views {
			if v.Name == ref.Name {
				return v
			}
		}
	}
	return nil
}

func findMatView(db *model.Database, ref model.ObjRef) *model.MaterializedView {
	for _, s := range db.Schemas {
		if s.Name != ref.Schema {
			continue
		}
		for _, mv := range s.MatViews {
			if mv.Name == ref.Name {
				return mv
			}
		}
	}
	return nil
}

// findTrigger looks a trigger up on a table or a view of db; the table is nil
// for a view trigger.
func findTrigger(db *model.Database, rel model.ObjRef, name string) (*model.Trigger, *model.Table) {
	switch rel.Kind {
	case model.KindView:
		if v := findView(db, rel); v != nil {
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
