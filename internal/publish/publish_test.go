package publish_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/publish"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// TestRun_PublishLifecycle walks the decisions the pipeline makes on a real
// target: apply and record a baseline; nothing to do the second time; refuse
// a target that drifted; let a dry run through (model only); refuse blocking
// hazards; apply with the hazards allowed and the drift accepted, recording a
// fresh baseline.
func TestRun_PublishLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()
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
	src := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(src, []byte("CREATE SCHEMA app; CREATE TABLE app.t(a int, b text);"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := publish.Request{Source: src, Plan: &deploy.Plan{}, Engine: engine.DefaultOptions(), RecordBaseline: true,
		Exec: execute.DefaultOptions(), Generator: "pg-dac test"}

	var planned, statements int
	obs := publish.Observer{
		Planned:   func(*engine.Result, []deploy.Script, []deploy.Script) { planned++ },
		Statement: func(int, int, sqlgen.Statement) { statements++ },
	}
	res, err := publish.Run(ctx, conn, req, obs)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if res.UpToDate || res.Applied != 1 || res.Baseline == nil || planned != 1 || statements != 1 {
		t.Fatalf("first publish: %+v planned=%d statements=%d", res, planned, statements)
	}

	res, err = publish.Run(ctx, conn, req, publish.Observer{})
	if err != nil || !res.UpToDate || res.Applied != 0 || res.Baseline != nil {
		t.Fatalf("second publish must find nothing to do and keep the baseline: %+v err=%v", res, err)
	}

	// A change outside pg-dac: refused, then let through by a dry run (model
	// only) and by AllowDrift.
	if _, err := conn.Exec(ctx, "CREATE TABLE app.extra(x int)"); err != nil {
		t.Fatal(err)
	}
	_, err = publish.Run(ctx, conn, req, publish.Observer{})
	var drifted *publish.DriftError
	if !errors.As(err, &drifted) || drifted.Changes != 1 || !publish.IsDrift(err) {
		t.Fatalf("drifted target must be refused: %v", err)
	}

	dry := req
	dry.DryRun = true
	dry.Engine.DropObjectsNotInSource = true
	var driftNote string
	res, err = publish.Run(ctx, conn, dry, publish.Observer{Drift: func(msg string) { driftNote = msg }})
	if err != nil || res.UpToDate || res.Migration.Empty() || driftNote == "" {
		t.Fatalf("dry run: %+v err=%v drift=%q", res, err, driftNote)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = 'app' AND tablename = 'extra'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("a dry run must not touch the target: n=%d err=%v", n, err)
	}

	// Dropping the extra table loses data: blocked until the hazards are
	// allowed.
	live := req
	live.AllowDrift = true
	live.Engine.DropObjectsNotInSource = true
	_, err = publish.Run(ctx, conn, live, publish.Observer{})
	var blocked *publish.BlockedError
	if !errors.As(err, &blocked) || len(blocked.MissingRoles) != 0 {
		t.Fatalf("data loss must be refused without AllowHazards: %v", err)
	}
	live.Engine.AllowHazards = true
	res, err = publish.Run(ctx, conn, live, publish.Observer{})
	if err != nil || res.Applied == 0 || res.Baseline == nil {
		t.Fatalf("publish over drift with hazards allowed: %+v err=%v", res, err)
	}
	if res, err = publish.Run(ctx, conn, req, publish.Observer{}); err != nil || !res.UpToDate {
		t.Fatalf("after the fresh baseline the target must be clean: %+v err=%v", res, err)
	}
}
