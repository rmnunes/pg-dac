package build_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
)

// Non-ASCII text in definitions, comments and identifiers must come out of a
// build exactly as it was authored, on every platform. On Windows the scratch
// cluster used to be WIN1252: with PGCLIENTENCODING=UTF8 an arrow failed the
// build, and without it non-ASCII text only survived because neither side
// converted the bytes.
func TestFromSQLKeepsNonASCIIText(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	// What a Windows user sets so psql sends UTF-8; against a WIN1252 scratch
	// server it turned the arrow below into a conversion error.
	t.Setenv("PGCLIENTENCODING", "UTF8")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	const body = `SELECT 'a — b → c ✓ 日本'::text`
	const comment = `em — dash, arrow →, check ✓`
	sql := `CREATE SCHEMA app;
CREATE TABLE app.t ("größe" int, note text);
COMMENT ON TABLE app.t IS '` + comment + `';
-- authored comment with an arrow → outside any statement
CREATE FUNCTION app.f() RETURNS text LANGUAGE sql AS $$ ` + body + ` $$;
COMMENT ON FUNCTION app.f() IS '` + comment + `';`

	db, _, err := build.FromSQL(ctx, sql, build.Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var found int
	for _, s := range db.Schemas {
		for _, tb := range s.Tables {
			found++
			if tb.Comment != comment {
				t.Errorf("table comment = %q, want %q", tb.Comment, comment)
			}
			if len(tb.Columns) == 0 || tb.Columns[0].Name != "größe" {
				t.Errorf("columns = %+v, want größe first", tb.Columns)
			}
		}
		for _, f := range s.Functions {
			found++
			if !strings.Contains(f.Definition, body) {
				t.Errorf("function definition lost its text:\n%s", f.Definition)
			}
			if f.Comment != comment {
				t.Errorf("function comment = %q, want %q", f.Comment, comment)
			}
		}
	}
	if found != 2 {
		t.Fatalf("expected the table and the function, found %d objects", found)
	}
}
