package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/validate"
)

// TestExcludedDependentViewsSurviveColumnChange: the target has a chain of
// views over a column whose type changes; the source has none of the views and
// the user excludes their drops. PostgreSQL will not alter the column while
// the views read it, so the engine must drop and recreate them with their
// current definitions — dependents first, dependencies last — and the plan
// must apply and converge on a real server.
func TestExcludedDependentViewsSurviveColumnChange(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	from, _, err := build.FromSQL(ctx, `
CREATE TABLE t(a int, b text);
CREATE VIEW v2 AS SELECT a, b FROM t;
CREATE VIEW v1 AS SELECT a FROM v2;
CREATE MATERIALIZED VIEW m1 AS SELECT a FROM v1;`, build.Options{})
	if err != nil {
		t.Fatalf("build from: %v", err)
	}
	to, _, err := build.FromSQL(ctx, `CREATE TABLE t(a bigint, b text);`, build.Options{})
	if err != nil {
		t.Fatalf("build to: %v", err)
	}

	opts := engine.DefaultOptions()
	opts.DropObjectsNotInSource = true
	opts.ExcludeIDs = map[string]bool{
		model.ObjRef{Kind: model.KindView, Schema: "public", Name: "v1"}.Key():    true,
		model.ObjRef{Kind: model.KindView, Schema: "public", Name: "v2"}.Key():    true,
		model.ObjRef{Kind: model.KindMatView, Schema: "public", Name: "m1"}.Key(): true,
	}
	res, err := engine.Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	idx := func(s string) int { return strings.Index(script, s) }
	dropM1, dropV1, dropV2 := idx(`DROP MATERIALIZED VIEW "public"."m1"`), idx(`DROP VIEW "public"."v1"`), idx(`DROP VIEW "public"."v2"`)
	alter := idx(`ALTER COLUMN "a" TYPE bigint`)
	createV2, createV1, createM1 := idx(`CREATE OR REPLACE VIEW "public"."v2"`), idx(`CREATE OR REPLACE VIEW "public"."v1"`), idx(`CREATE MATERIALIZED VIEW "public"."m1"`)
	for name, i := range map[string]int{"drop m1": dropM1, "drop v1": dropV1, "drop v2": dropV2, "alter": alter, "create v2": createV2, "create v1": createV1, "create m1": createM1} {
		if i < 0 {
			t.Errorf("missing %s in script:\n%s", name, script)
		}
	}
	if t.Failed() {
		return
	}
	if !(dropM1 < dropV1 && dropV1 < dropV2 && dropV2 < alter && alter < createV2 && createV2 < createV1 && createV1 < createM1) {
		t.Errorf("wrong order (dependents dropped first, recreated last):\n%s", script)
	}
	vr, err := validate.Plan(ctx, from, to, opts)
	if err != nil {
		t.Fatalf("migration failed to apply: %v\n%s", err, script)
	}
	if !vr.Converged {
		t.Fatalf("did not converge; residual:\n%s", vr.Residual.Script(true))
	}
}

// TestExcludedFunctionOverViewIsRecreated reproduces the DEV failure: the
// target has a view over a column whose type changes and a function whose
// signature returns that view's row type; the source has neither and the user
// excludes both drops. PostgreSQL refuses to drop the view while the function
// exists, so the function must be dropped first and recreated after the view
// comes back — both kept with their current definitions.
func TestExcludedFunctionOverViewIsRecreated(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	from, _, err := build.FromSQL(ctx, `
CREATE TABLE t(a int, b text);
CREATE VIEW v AS SELECT a, b FROM t;
CREATE FUNCTION f(ids text[]) RETURNS SETOF v LANGUAGE sql AS 'SELECT * FROM v';`, build.Options{})
	if err != nil {
		t.Fatalf("build from: %v", err)
	}
	to, _, err := build.FromSQL(ctx, `CREATE TABLE t(a bigint, b text);`, build.Options{})
	if err != nil {
		t.Fatalf("build to: %v", err)
	}
	opts := engine.DefaultOptions()
	opts.DropObjectsNotInSource = true
	// The function's identity carries its parameter names ("f(ids text[])"),
	// so take the key from the model rather than spelling it.
	fnKey := from.Schemas[0].Functions[0].Ref().Key()
	opts.ExcludeIDs = map[string]bool{
		model.ObjRef{Kind: model.KindView, Schema: "public", Name: "v"}.Key(): true,
		fnKey: true,
	}
	res, err := engine.Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	idx := func(s string) int { return strings.Index(script, s) }
	dropF, dropV, alter, createV, createF := idx("DROP FUNCTION"), idx(`DROP VIEW "public"."v"`), idx(`ALTER COLUMN "a" TYPE bigint`), idx(`CREATE OR REPLACE VIEW "public"."v"`), idx("CREATE OR REPLACE FUNCTION")
	for name, i := range map[string]int{"drop f": dropF, "drop v": dropV, "alter": alter, "create v": createV, "create f": createF} {
		if i < 0 {
			t.Errorf("missing %s in script:\n%s", name, script)
		}
	}
	if t.Failed() {
		return
	}
	if !(dropF < dropV && dropV < alter && alter < createV && createV < createF) {
		t.Errorf("wrong order (function before view on the way down, after it on the way up):\n%s", script)
	}
	var fnReason string
	for _, c := range res.ObjectChanges() {
		if c.ObjectType == model.KindFunction {
			fnReason = c.Reason
		}
	}
	if !strings.Contains(fnReason, "recreated because it depends on view") {
		t.Errorf("function reason = %q", fnReason)
	}
	vr, err := validate.Plan(ctx, from, to, opts)
	if err != nil {
		t.Fatalf("migration failed to apply: %v\n%s", err, script)
	}
	if !vr.Converged {
		t.Fatalf("did not converge; residual:\n%s", vr.Residual.Script(true))
	}
}
