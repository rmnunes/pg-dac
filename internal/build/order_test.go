package build_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
)

// A project's statements may appear in any order: a view before its table, a
// foreign key before the referenced table, a trigger before its function. The
// build defers what fails and retries it, like a declarative SQL project.
func TestFromSQLIsOrderInsensitive(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	const sql = `
CREATE VIEW v2 AS SELECT a FROM v1;
CREATE VIEW v1 AS SELECT a FROM t;
ALTER TABLE c ADD CONSTRAINT c_t_fk FOREIGN KEY (t_id) REFERENCES t(id);
CREATE TRIGGER tg BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION tf();
CREATE TABLE c(id int PRIMARY KEY, t_id int);
CREATE FUNCTION tf() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$;
CREATE TABLE t(id int PRIMARY KEY, a int);
GRANT SELECT ON t TO order_reader;
DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='order_reader') THEN CREATE ROLE order_reader NOLOGIN; END IF; END$$;`
	db, _, err := build.FromSQL(ctx, sql, build.Options{})
	if err != nil {
		t.Fatalf("order-insensitive build failed: %v", err)
	}
	var tables, views, functions, grants int
	for _, s := range db.Schemas {
		for _, tb := range s.Tables {
			tables++
			grants += len(tb.Grants)
			if tb.Name == "t" && len(tb.Triggers) != 1 {
				t.Errorf("trigger on t missing: %+v", tb.Triggers)
			}
			if tb.Name == "c" && len(tb.Constraints) != 2 {
				t.Errorf("c should have its PK and FK, got %d", len(tb.Constraints))
			}
		}
		views += len(s.Views)
		functions += len(s.Functions)
	}
	if tables != 2 || views != 2 || functions != 1 || grants != 1 {
		t.Fatalf("model incomplete: tables=%d views=%d functions=%d grants=%d", tables, views, functions, grants)
	}

	// A statement that can never apply is reported, with the others still built.
	_, _, err = build.FromSQL(ctx, `CREATE TABLE ok(a int); CREATE VIEW bad AS SELECT nope FROM missing;`, build.Options{})
	if err == nil || !strings.Contains(err.Error(), "CREATE VIEW bad") || !strings.Contains(err.Error(), "1 statement(s) failed") {
		t.Fatalf("expected a report of the failing statement, got %v", err)
	}
}
