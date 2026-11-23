package drift_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/drift"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
)

func TestDrift_RecordAndCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
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
	if _, err := conn.Exec(ctx, "CREATE SCHEMA app; CREATE TABLE app.t(a int);"); err != nil {
		t.Fatal(err)
	}

	cur, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := drift.Check(ctx, conn, cur)
	if err != nil || rep.Baseline != nil || rep.Drifted {
		t.Fatalf("no baseline yet: %+v err=%v", rep, err)
	}

	bl, err := drift.Record(ctx, conn, cur, "test.pgdac v1", "pg-dac test")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := drift.Latest(ctx, conn)
	if err != nil || latest == nil || latest.SHA256 != bl.SHA256 || latest.Source != "test.pgdac v1" {
		t.Fatalf("latest: %+v err=%v", latest, err)
	}

	// The baseline table itself must not count as drift (pgdac is excluded
	// from models), so re-reading right away is clean.
	cur2, _ := introspect.Introspect(ctx, conn, introspect.Options{})
	rep, err = drift.Check(ctx, conn, cur2)
	if err != nil || rep.Drifted || rep.Baseline == nil {
		t.Fatalf("fresh baseline should not drift: %+v err=%v", rep, err)
	}

	if _, err := conn.Exec(ctx, "ALTER TABLE app.t ADD COLUMN b text; CREATE VIEW app.v AS SELECT a FROM app.t;"); err != nil {
		t.Fatal(err)
	}
	cur3, _ := introspect.Introspect(ctx, conn, introspect.Options{})
	rep, err = drift.Check(ctx, conn, cur3)
	if err != nil || !rep.Drifted || rep.Changes == nil {
		t.Fatalf("expected drift: %+v err=%v", rep, err)
	}
	if n := len(rep.Changes.Plan.Ops); n != 2 {
		t.Errorf("expected 2 drift operations (column + view), got %d:\n%s", n, rep.Changes.Script(true))
	}

	// A new baseline absorbs the drift.
	if _, err := drift.Record(ctx, conn, cur3, "manual", "pg-dac test"); err != nil {
		t.Fatal(err)
	}
	rep, _ = drift.Check(ctx, conn, cur3)
	if rep.Drifted {
		t.Error("re-baselined target must not drift")
	}
}

// A database published as the desired state must not leave its password in
// the target: the baseline records only the server it names, and a baseline
// written before that rule is redacted when it is read back.
func TestDrift_BaselineSourceHasNoPassword(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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

	db := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "public"}}}
	b, err := drift.Record(ctx, conn, db, "postgres://app:s3cret@dev.example:5432/app", "test")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := conn.QueryRow(ctx, `SELECT source FROM pgdac.baseline WHERE id = $1`, b.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "s3cret") || stored != "dev.example:5432/app" {
		t.Fatalf("baseline source stored as %q", stored)
	}

	// A row from an older version, password and all.
	if _, err := conn.Exec(ctx, `UPDATE pgdac.baseline SET source = 'postgres://app:s3cret@dev.example/app' WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	latest, err := drift.Latest(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(latest.Source, "s3cret") {
		t.Fatalf("an old baseline's password was handed back: %q", latest.Source)
	}
}
