package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

// depFixture: table app.t(a <colType>, b text), view v reading t.a, view w on v.
func depFixture(colType string) *model.Database {
	t := &model.Table{Schema: "app", Name: "t", Columns: []*model.Column{{Name: "a", Type: colType}, {Name: "b", Type: "text"}}}
	v := &model.View{Schema: "app", Name: "v", Definition: "SELECT a FROM app.t",
		DependsOn:  []model.ObjRef{t.Ref()},
		ColumnDeps: []model.ColumnRef{{Schema: "app", Table: "t", Column: "a"}},
		Columns:    []model.ViewColumn{{Name: "a", Type: colType}}}
	w := &model.View{Schema: "app", Name: "w", Definition: "SELECT a FROM app.v",
		DependsOn:  []model.ObjRef{v.Ref()},
		ColumnDeps: []model.ColumnRef{{Schema: "app", Table: "v", Column: "a"}},
		Columns:    []model.ViewColumn{{Name: "a", Type: colType}}}
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{t}, Views: []*model.View{v, w}}}}
}

func ordered(t *testing.T, script string, needles ...string) {
	t.Helper()
	last := -1
	for _, n := range needles {
		i := indexOf(t, script, n)
		if i < last {
			t.Fatalf("expected %q to come after the previous statement; script:\n%s", n, script)
		}
		last = i
	}
}

// A column type change is blocked by every view that reads the column,
// transitively: the views are dropped (dependents first), the column altered,
// and the views recreated (dependencies first).
func TestExpandDependents_AlterTypeUnderViews(t *testing.T) {
	res, err := Compare(depFixture("integer"), depFixture("bigint"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ordered(t, res.Script(true),
		`DROP VIEW "app"."w"`, `DROP VIEW "app"."v"`,
		`ALTER COLUMN "a" TYPE bigint`,
		`CREATE OR REPLACE VIEW "app"."v"`, `CREATE OR REPLACE VIEW "app"."w"`)
	if self, _ := Compare(depFixture("bigint"), depFixture("bigint"), DefaultOptions()); !self.Empty() {
		t.Errorf("self-compare must stay empty:\n%s", self.Script(true))
	}
}

// A view whose columns shrink is recreated, and so is the view on top of it.
func TestExpandDependents_IncompatibleViewRecreatesDependents(t *testing.T) {
	from := depFixture("integer")
	to := depFixture("integer")
	from.Schemas[0].Views[0].Definition = "SELECT a, b FROM app.t"
	from.Schemas[0].Views[0].Columns = []model.ViewColumn{{Name: "a", Type: "integer"}, {Name: "b", Type: "text"}}
	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ordered(t, res.Script(true),
		`DROP VIEW "app"."w"`, `DROP VIEW "app"."v"`,
		`CREATE OR REPLACE VIEW "app"."v"`, `CREATE OR REPLACE VIEW "app"."w"`)
}

// A function whose return type changes is dropped and recreated, and a view
// calling it is recreated around that.
func TestExpandDependents_FunctionReturnTypeUnderView(t *testing.T) {
	mk := func(ret string) *model.Database {
		f := &model.Function{Schema: "app", Name: "f", Kind: "function", ReturnType: ret, Arguments: "",
			Definition: "CREATE OR REPLACE FUNCTION app.f()\n RETURNS " + ret + "\n LANGUAGE sql\nAS $function$SELECT 1$function$"}
		v := &model.View{Schema: "app", Name: "v", Definition: "SELECT app.f() AS x", DependsOn: []model.ObjRef{f.Ref()},
			Columns: []model.ViewColumn{{Name: "x", Type: ret}}}
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Functions: []*model.Function{f}, Views: []*model.View{v}}}}
	}
	res, err := Compare(mk("integer"), mk("bigint"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ordered(t, res.Script(true),
		`DROP VIEW "app"."v"`, `DROP FUNCTION "app"."f"()`,
		`CREATE OR REPLACE FUNCTION app.f()`, `CREATE OR REPLACE VIEW "app"."v"`)
}

// Dropping a table a view currently reads: the view is recreated in its new
// form after the table is gone (its old form is dropped first).
func TestExpandDependents_DropTableUnderRewrittenView(t *testing.T) {
	tbl := func(name string) *model.Table {
		return &model.Table{Schema: "app", Name: name, Columns: []*model.Column{{Name: "a", Type: "integer"}}}
	}
	view := func(on string) *model.View {
		return &model.View{Schema: "app", Name: "v", Definition: "SELECT a FROM app." + on,
			DependsOn:  []model.ObjRef{{Kind: model.KindTable, Schema: "app", Name: on}},
			ColumnDeps: []model.ColumnRef{{Schema: "app", Table: on, Column: "a"}},
			Columns:    []model.ViewColumn{{Name: "a", Type: "integer"}}}
	}
	from := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{tbl("t"), tbl("u")}, Views: []*model.View{view("t")}}}}
	to := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{tbl("u")}, Views: []*model.View{view("u")}}}}
	opts := DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	ordered(t, res.Script(true), `DROP VIEW "app"."v"`, `DROP TABLE "app"."t"`, `CREATE OR REPLACE VIEW "app"."v"`)
}

// A function whose signature uses a view's row type, with a new body in the
// desired state: the differ emits only its CREATE OR REPLACE, so this pass
// adds the drop (before the view goes) and the differ's create lands after the
// view is back — one create, with the reason on the object.
func TestExpandDependents_FunctionOverViewWithNewBody(t *testing.T) {
	mk := func(colType, body string) *model.Database {
		tbl := &model.Table{Schema: "app", Name: "t", Columns: []*model.Column{{Name: "a", Type: colType}}}
		v := &model.View{Schema: "app", Name: "v", Definition: "SELECT a FROM app.t",
			DependsOn:  []model.ObjRef{tbl.Ref()},
			ColumnDeps: []model.ColumnRef{{Schema: "app", Table: "t", Column: "a"}},
			Columns:    []model.ViewColumn{{Name: "a", Type: colType}}}
		f := &model.Function{Schema: "app", Name: "f", Kind: "function", ReturnType: "SETOF app.v",
			Definition: "CREATE OR REPLACE FUNCTION app.f()\n RETURNS SETOF app.v\n LANGUAGE sql\nAS $function$" + body + "$function$",
			DependsOn:  []model.ObjRef{v.Ref()}}
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{tbl}, Views: []*model.View{v}, Functions: []*model.Function{f}}}}
	}
	res, err := Compare(mk("integer", "SELECT * FROM app.v"), mk("bigint", "SELECT * FROM app.v WHERE a > 0"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	ordered(t, script,
		`DROP FUNCTION "app"."f"()`, `DROP VIEW "app"."v"`,
		`ALTER COLUMN "a" TYPE bigint`,
		`CREATE OR REPLACE VIEW "app"."v"`, `CREATE OR REPLACE FUNCTION app.f()`)
	if n := strings.Count(script, "CREATE OR REPLACE FUNCTION app.f()"); n != 1 {
		t.Errorf("the function must be created once, got %d:\n%s", n, script)
	}
	changes, err := res.ObjectChanges()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.ObjectType == model.KindFunction && !strings.Contains(c.Reason, "recreated because it depends on view") {
			t.Errorf("function reason = %q", c.Reason)
		}
	}
}
