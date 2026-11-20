package plan

import (
	"testing"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// position returns the index in the plan of the first op of the given type
// whose Owner name and Sub match, or -1.
func position(p *Plan, typ diff.OpType, owner, sub string) int {
	for i, op := range p.Ops {
		if op.Type == typ && op.Owner.Name == owner && op.Sub == sub {
			return i
		}
	}
	return -1
}

func before(t *testing.T, p *Plan, what string, a, b int) {
	t.Helper()
	if a < 0 || b < 0 || a >= b {
		t.Errorf("%s: positions %d, %d in %v", what, a, b, summary(p))
	}
}

func summary(p *Plan) []string {
	out := make([]string, 0, len(p.Ops))
	for _, op := range p.Ops {
		out = append(out, string(op.Type)+":"+op.Owner.Name+"#"+op.Sub)
	}
	return out
}

// TestBuild_IndexBands: a plain index drop runs before a column drop (the
// index may be on that column), an index rebuild and a new index after it
// (the new definition may use columns the migration adds or retypes), and a
// rebuild after the table's column changes.
func TestBuild_IndexBands(t *testing.T) {
	tbl := &model.Table{Schema: "app", Name: "t"}
	ix := func(name string) *model.Index {
		return &model.Index{Name: name, Table: "t", Definition: "CREATE INDEX " + name + " ON app.t USING btree (a)"}
	}
	cs := &diff.ChangeSet{Ops: []diff.Op{
		{Type: diff.OpReplaceIndex, Owner: tbl.Ref(), Sub: "t_ab", Table: tbl, Index: ix("t_ab"), PrevIndex: ix("t_ab"), TempIndexName: "t_ab_pgdac_new"},
		{Type: diff.OpCreateIndex, Owner: tbl.Ref(), Sub: "t_new", Table: tbl, Index: ix("t_new")},
		{Type: diff.OpAlterColumnType, Owner: tbl.Ref(), Sub: "a", Table: tbl, Column: &model.Column{Name: "a", Type: "bigint"}, PrevColumn: &model.Column{Name: "a", Type: "integer"}},
		{Type: diff.OpDropColumn, Owner: tbl.Ref(), Sub: "b", Table: tbl, Column: &model.Column{Name: "b", Type: "integer"}},
		{Type: diff.OpDropIndex, Owner: tbl.Ref(), Sub: "t_old", Table: tbl, Index: ix("t_old")},
	}}
	p := Build(cs)
	if len(p.Cyclic) != 0 || len(p.Ops) != len(cs.Ops) {
		t.Fatalf("plan: %v cyclic=%d", summary(p), len(p.Cyclic))
	}
	dropIdx := position(p, diff.OpDropIndex, "t", "t_old")
	dropCol := position(p, diff.OpDropColumn, "t", "b")
	alter := position(p, diff.OpAlterColumnType, "t", "a")
	replace := position(p, diff.OpReplaceIndex, "t", "t_ab")
	create := position(p, diff.OpCreateIndex, "t", "t_new")
	before(t, p, "drop index before drop column", dropIdx, dropCol)
	before(t, p, "drop column before rebuild", dropCol, replace)
	before(t, p, "alter column before rebuild", alter, replace)
	before(t, p, "drop column before new index", dropCol, create)
}

// TestBuild_FunctionOverViewEdges: a function whose signature uses a view's
// row type is dropped before the view and recreated after it, although the
// bands alone would drop the view first — the edges derived from
// Function.DependsOn decide.
func TestBuild_FunctionOverViewEdges(t *testing.T) {
	tbl := model.ObjRef{Kind: model.KindTable, Schema: "app", Name: "t"}
	v := &model.View{Schema: "app", Name: "v", Definition: "SELECT a FROM app.t", DependsOn: []model.ObjRef{tbl}}
	f := &model.Function{Schema: "app", Name: "f", Kind: "function", ReturnType: "SETOF app.v",
		Definition: "CREATE OR REPLACE FUNCTION app.f()\n RETURNS SETOF app.v\n LANGUAGE sql\nAS $function$SELECT * FROM app.v$function$",
		DependsOn:  []model.ObjRef{v.Ref()}}
	cs := &diff.ChangeSet{Ops: []diff.Op{
		{Type: diff.OpDropView, Owner: v.Ref(), View: v},
		{Type: diff.OpCreateView, Owner: v.Ref(), View: v},
		{Type: diff.OpDropFunction, Owner: f.Ref(), Function: f},
		{Type: diff.OpCreateFunction, Owner: f.Ref(), Function: f},
	}}
	p := Build(cs)
	if len(p.Cyclic) != 0 {
		t.Fatalf("cycle: %v", summary(p))
	}
	dropF, dropV := position(p, diff.OpDropFunction, f.Signature(), ""), position(p, diff.OpDropView, "v", "")
	createV, createF := position(p, diff.OpCreateView, "v", ""), position(p, diff.OpCreateFunction, f.Signature(), "")
	before(t, p, "drop function before drop view", dropF, dropV)
	before(t, p, "drop view before create view", dropV, createV)
	before(t, p, "create view before create function", createV, createF)
}
