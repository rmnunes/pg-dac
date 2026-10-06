package refactor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rodrigonunes/pg-dac/internal/build"
	"github.com/rodrigonunes/pg-dac/internal/devdb"
	"github.com/rodrigonunes/pg-dac/internal/engine"
	"github.com/rodrigonunes/pg-dac/internal/execute"
	"github.com/rodrigonunes/pg-dac/internal/introspect"
	"github.com/rodrigonunes/pg-dac/internal/refactor"
)

// A rename recorded in the refactorlog must be applied as ALTER ... RENAME so
// the data survives, the target must remember it, and the result must converge
// on the desired state.
func TestRefactorlog_RenamesPreserveData(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	fromSQL := `CREATE SCHEMA app;
CREATE TABLE app.t(id serial PRIMARY KEY, b int);
CREATE INDEX t_b_idx ON app.t(b);
CREATE VIEW app.v AS SELECT id, b FROM app.t;
CREATE TABLE app.u(tid int REFERENCES app.t(id));`
	toSQL := `CREATE SCHEMA app;
CREATE TABLE app.t2(id serial PRIMARY KEY, bee int);
CREATE INDEX t2_bee_idx ON app.t2(bee);
CREATE VIEW app.v AS SELECT id, bee FROM app.t2;
CREATE TABLE app.u(tid int REFERENCES app.t2(id));`
	log := &refactor.Log{Operations: []refactor.Op{
		{ID: "r1", Kind: refactor.KindTable, Schema: "app", From: "t", To: "t2"},
		{ID: "r2", Kind: refactor.KindColumn, Schema: "app", Table: "t2", From: "b", To: "bee"},
		{ID: "r3", Kind: refactor.KindSequence, Schema: "app", From: "t_id_seq", To: "t2_id_seq"},
		{ID: "r4", Kind: refactor.KindIndex, Schema: "app", From: "t_b_idx", To: "t2_bee_idx"},
		{ID: "r5", Kind: refactor.KindConstraint, Schema: "app", Table: "t2", From: "t_pkey", To: "t2_pkey"},
	}}

	from, _, err := build.FromSQL(ctx, fromSQL, build.Options{})
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := build.FromSQL(ctx, toSQL, build.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// The plan must not drop/recreate the table or its column.
	renames, warns := refactor.Pending(from, log, nil)
	if len(renames) != 5 || len(warns) != 0 {
		t.Fatalf("pending: %d renames, warnings %v", len(renames), warns)
	}
	opts := engine.DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, err := engine.Compare(refactor.ApplyToModel(from, renames), to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	for _, bad := range []string{`DROP TABLE "app"."t"`, `CREATE TABLE "app"."t2"`, `DROP COLUMN`, `ADD COLUMN "bee"`} {
		if strings.Contains(script, bad) {
			t.Errorf("renamed objects must not be dropped/recreated; found %q in:\n%s", bad, script)
		}
	}

	// Replay for real: materialize "from" with data, run the renames, then the
	// migration; the row must still be there and the schema must converge.
	sc, err := devdb.Acquire(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Release()
	conn, err := pgx.Connect(ctx, sc.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	baseline, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mat, err := engine.Compare(baseline, from, engine.Options{Online: false, AllowHazards: true, DropObjectsNotInSource: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := execute.ApplyStatements(ctx, conn, mat.Statements); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO app.t(b) VALUES (42)"); err != nil {
		t.Fatal(err)
	}

	applied, err := refactor.ReadApplied(ctx, conn)
	if err != nil || len(applied) != 0 {
		t.Fatalf("applied before: %v %v", applied, err)
	}
	live, _ := introspect.Introspect(ctx, conn, introspect.Options{})
	renames, _ = refactor.Pending(live, log, applied)
	if err := execute.ApplyStatements(ctx, conn, refactor.Statements(renames)); err != nil {
		t.Fatalf("renames: %v", err)
	}
	applied, err = refactor.ReadApplied(ctx, conn)
	if err != nil || len(applied) != 5 {
		t.Fatalf("applied after: %v %v", applied, err)
	}
	renamed, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := refactor.Pending(renamed, log, applied); len(again) != 0 {
		t.Errorf("nothing should be pending after the renames ran, got %d", len(again))
	}
	mig, err := engine.Compare(renamed, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := execute.ApplyStatements(ctx, conn, mig.Statements); err != nil {
		t.Fatalf("migration after renames: %v\n%s", err, mig.Script(true))
	}
	var bee int
	if err := conn.QueryRow(ctx, "SELECT bee FROM app.t2").Scan(&bee); err != nil || bee != 42 {
		t.Errorf("data must survive the rename: bee=%d err=%v", bee, err)
	}
	got, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if residual, _ := engine.Compare(got, to, opts); !residual.Empty() {
		t.Errorf("target should converge on the desired state; residual:\n%s", residual.Script(true))
	}
	// pgdac is pg-dac's own schema and never part of a model.
	for _, s := range got.Schemas {
		if s.Name == "pgdac" {
			t.Error("the pgdac bookkeeping schema must not be introspected into the model")
		}
	}
}
