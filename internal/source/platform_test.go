package source_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/dbconn"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/source"
)

// A project that targets PostgreSQL 18 compared against a PostgreSQL 16
// database: normalized on 18, the project carries btree_gist at the version
// 18 ships, which 16 does not have. The comparison is refused with the way
// out, and normalizing for 16 compares like for like.
func TestNewerProjectAgainstOlderDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres 16 and 18")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	const schema = `CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE SCHEMA app;
CREATE TABLE app.booking (room int, during tstzrange);
`
	dir := t.TempDir()
	manifest := "name: app\nversion: 1.0.0\ntargetMajor: 18\ninclude: [\"*.sql\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "pgdac.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}

	live, err := devdb.Acquire(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Release()
	conn, err := dbconn.Connect(ctx, live.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, schema)
	conn.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}

	resolve := func(spec string, major int) *engine.Result {
		t.Helper()
		opts := source.Options{TargetMajor: major}
		from, _, err := source.Resolve(ctx, live.ConnString, opts)
		if err != nil {
			t.Fatal(err)
		}
		to, _, err := source.Resolve(ctx, spec, opts)
		if err != nil {
			t.Fatal(err)
		}
		res, err := engine.Compare(from, to, engine.DefaultOptions())
		if err != nil {
			if major != 0 {
				t.Fatalf("compare for PostgreSQL %d: %v", major, err)
			}
			if !strings.Contains(err.Error(), "--target-major 16") {
				t.Fatalf("refusal does not name the fix: %v", err)
			}
			return nil
		}
		return res
	}

	if res := resolve(dir, 0); res != nil {
		t.Fatalf("the project's PostgreSQL 18 normalization was compared against a 16 database:\n%s", res.Script(true))
	}
	if res := resolve(dir, 16); !res.Empty() {
		t.Fatalf("normalized for 16, the same schema still differs:\n%s", res.Script(true))
	}
}
