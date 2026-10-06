package engine

import (
	"fmt"

	"github.com/rodrigonunes/pg-dac/internal/diff"
	"github.com/rodrigonunes/pg-dac/internal/model"
)

// expandDependents makes blocking operations applicable by recreating the
// objects that currently depend on what they change. PostgreSQL refuses to
//
//   - change the type of, or drop, a column a view reads;
//   - drop a table, view or function another view still references;
//   - drop a function a trigger executes;
//
// and the differ deliberately emits those changes as plain ops. This pass walks
// the "from" model's dependency data (View.ColumnDeps / DependsOn,
// Trigger.DependsOn), finds every dependent — transitively — and adds a DROP of
// its current form plus a CREATE of its desired form (or of its current form,
// when the desired state does not mention it and it can still exist). The
// planner then brackets the blocking op with those drops and creates via the
// usual bands and edges. Dependents that reference an object that disappears
// for good are dropped only. It runs after the drop/include filters so it only
// reacts to changes that will really be applied, and its own drops are always
// paired with a create of the same object, so they are never filtered away.
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
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpAlterColumnType:
			blockedCols[colKey(op.Owner, op.Sub)] = true
		case diff.OpDropColumn:
			blockedCols[colKey(op.Owner, op.Sub)] = true
			droppedCols[colKey(op.Owner, op.Sub)] = true
		case diff.OpDropView, diff.OpDropMatView, diff.OpDropFunction, diff.OpDropTable:
			dropped[op.Owner.Key()] = true
		case diff.OpCreateView, diff.OpCreateMatView, diff.OpCreateFunction, diff.OpCreateTable:
			created[op.Owner.Key()] = true
		case diff.OpDropTrigger:
			trigDropped[colKey(op.Owner, op.Sub)] = true
		case diff.OpCreateTrigger:
			trigCreated[colKey(op.Owner, op.Sub)] = true
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

	// recreate drops a dependent relation and recreates it in its desired form.
	recreate := func(d dependentRel, cause string) {
		key := d.ref.Key()
		if dropped[key] {
			return // already dropped by the differ or an earlier step
		}
		dropped[key] = true
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
			if created[key] {
				return // the differ already recreates it in its desired form
			}
			if desired := findView(to, d.ref); desired != nil {
				added = append(added, diff.Op{Type: diff.OpCreateView, Owner: d.ref, View: desired, Reason: reason})
				created[key] = true
			} else if !invalid {
				added = append(added, diff.Op{Type: diff.OpCreateView, Owner: d.ref, View: d.view,
					Reason: reason + " (kept with its current definition; it is not part of the desired state)"})
				created[key] = true
			}
		case d.mview != nil:
			added = append(added, diff.Op{Type: diff.OpDropMatView, Owner: d.ref, MatView: d.mview, Reason: reason})
			if created[key] {
				return
			}
			if desired := findMatView(to, d.ref); desired != nil {
				added = append(added, diff.Op{Type: diff.OpCreateMatView, Owner: d.ref, MatView: desired, Reason: reason})
				created[key] = true
			} else if !invalid {
				added = append(added, diff.Op{Type: diff.OpCreateMatView, Owner: d.ref, MatView: d.mview,
					Reason: reason + " (kept with its current definition; it is not part of the desired state)"})
				created[key] = true
			}
		}
	}

	// Column-level seeds: views reading a column whose type changes or that is dropped.
	for ck := range blockedCols {
		for _, d := range idx.byColumn[ck] {
			recreate(d, "column "+ck[len(prefixLen(ck)):])
		}
	}
	// Object-level seeds and everything they cascade to.
	for len(frontier) > 0 {
		k := frontier[0]
		frontier = frontier[1:]
		for _, d := range idx.byObject[k] {
			recreate(d, k)
		}
		// Triggers executing a dropped function.
		for _, t := range idx.triggersByFn[k] {
			tk := colKey(t.table.Ref(), t.tr.Name)
			if trigDropped[tk] || gone(t.table.Ref().Key()) {
				continue // already handled, or the whole table goes away
			}
			trigDropped[tk] = true
			reason := "recreated because it executes " + k
			added = append(added, diff.Op{Type: diff.OpDropTrigger, Owner: t.table.Ref(), Sub: t.tr.Name, Table: t.table, Trigger: t.tr, Reason: reason})
			if trigCreated[tk] {
				continue
			}
			if desired, tbl := findTrigger(to, t.table.Ref(), t.tr.Name); desired != nil {
				added = append(added, diff.Op{Type: diff.OpCreateTrigger, Owner: t.table.Ref(), Sub: t.tr.Name, Table: tbl, Trigger: desired, Reason: reason})
				trigCreated[tk] = true
			} else if created[k] {
				added = append(added, diff.Op{Type: diff.OpCreateTrigger, Owner: t.table.Ref(), Sub: t.tr.Name, Table: t.table, Trigger: t.tr,
					Reason: reason + " (kept with its current definition; it is not part of the desired state)"})
				trigCreated[tk] = true
			}
		}
	}

	if len(added) == 0 {
		return cs
	}
	out := &diff.ChangeSet{Ops: make([]diff.Op, 0, len(cs.Ops)+len(added))}
	out.Ops = append(out.Ops, cs.Ops...)
	out.Ops = append(out.Ops, added...)
	return out
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

type dependentTrigger struct {
	table *model.Table
	tr    *model.Trigger
}

type dependentIndex struct {
	byObject     map[string][]dependentRel // relation/function/view key -> relations depending on it
	byColumn     map[string][]dependentRel // table key + "#" + column -> relations reading it
	triggersByFn map[string][]dependentTrigger
}

func indexDependents(db *model.Database) dependentIndex {
	idx := dependentIndex{
		byObject:     map[string][]dependentRel{},
		byColumn:     map[string][]dependentRel{},
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
					idx.triggersByFn[dep.Key()] = append(idx.triggersByFn[dep.Key()], dependentTrigger{table: t, tr: tr})
				}
			}
		}
	}
	return idx
}

// colKey identifies a column (or any table sub-object) by its table and name,
// matching the differ's Owner + Sub.
func colKey(table model.ObjRef, sub string) string { return table.Key() + "#" + sub }

func colRefKey(c model.ColumnRef) string {
	return colKey(model.ObjRef{Kind: model.KindTable, Schema: c.Schema, Name: c.Table}, c.Column)
}

// prefixLen returns the length of the "kind:" prefix of a key so the human
// readable cause can show the qualified name only.
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

func findTrigger(db *model.Database, table model.ObjRef, name string) (*model.Trigger, *model.Table) {
	t := db.Table(table.Schema, table.Name)
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
