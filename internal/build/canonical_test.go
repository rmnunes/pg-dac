package build_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/introspect"
)

// A database created by hand from authored SQL holds PostgreSQL's first
// deparse of each expression; a project built by pg-dac holds the fixed point.
// Comparing the raw introspection against the project reports differences that
// are only a matter of deparse form; canonicalizing the live model first makes
// the same comparison empty.
func TestCanonicalizeLiveModelRemovesDeparseFormDifferences(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	const sql = `
CREATE TABLE log(id int PRIMARY KEY, code varchar(2), review_id text);
CREATE INDEX ix_log_final ON log(review_id) WHERE code IN ('F', 'FE');
CREATE VIEW v AS SELECT id::text AS id, code FROM log WHERE code IN ('F', 'FE');
CREATE FUNCTION f() RETURNS text LANGUAGE sql AS 'SELECT ''x''';`

	// A "hand-made" database: the SQL applied directly.
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
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
	raw, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// The same SQL as a project (canonical fixed point).
	project, _, err := build.FromSQL(ctx, sql, build.Options{})
	if err != nil {
		t.Fatal(err)
	}

	opts := engine.DefaultOptions()
	rawRes, err := engine.Compare(raw, project, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rawRes.Empty() {
		t.Skip("this PostgreSQL deparses the authored form to its fixed point directly; nothing to canonicalize")
	}

	canon, err := build.Canonicalize(ctx, raw, build.Options{})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	res, err := engine.Compare(canon, project, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty() {
		t.Fatalf("canonical live model still differs from the project:\n%s", res.Script(true))
	}
	// Provenance survives: the exact server version is the live server's.
	if canon.TargetVersion != raw.TargetVersion {
		t.Errorf("TargetVersion changed: %d -> %d", raw.TargetVersion, canon.TargetVersion)
	}
	// Memoized: a second call is served from the cache and equal.
	again, err := build.Canonicalize(ctx, raw, build.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := canon.Marshal()
	b, _ := again.Marshal()
	if string(a) != string(b) {
		t.Error("cached canonical model differs from the computed one")
	}
}
