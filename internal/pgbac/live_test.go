package pgbac_test

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/pgbac"
)

// Export a populated database, import the archive into an empty one: rows,
// identity and serial positions, generated values, foreign keys, triggers,
// a materialized view's contents and grants all come across, and the two
// schemas compare equal.
func TestExportImportRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	src, err := devdb.Acquire(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Release()
	dst, err := devdb.Acquire(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Release()

	a, err := pgx.Connect(ctx, src.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if _, err := a.Exec(ctx, `
DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='bac_reader') THEN CREATE ROLE bac_reader NOLOGIN; END IF; END$$;
CREATE SCHEMA app;
`+parentTable()+`
CREATE TABLE app.child(id serial PRIMARY KEY, parent_id int NOT NULL REFERENCES app.parent(id), note text, "Odd Col" int DEFAULT 7);
CREATE FUNCTION app.tf() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN NEW.note := coalesce(NEW.note, '') || '!'; RETURN NEW; END$$;
CREATE TRIGGER tg BEFORE INSERT ON app.child FOR EACH ROW EXECUTE FUNCTION app.tf();
INSERT INTO app.parent(name) VALUES ('a'), ('b'), ('c, with "quotes"
and a newline');
`+parentFill()+`
INSERT INTO app.child(parent_id, note) VALUES (1, 'x'), (2, NULL), (3, 'z');
CREATE MATERIALIZED VIEW app.mv AS SELECT count(*) AS n FROM app.child WITH DATA;
GRANT SELECT ON app.parent TO bac_reader;
CREATE TABLE app.empty_table(id int PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "x.pgbac")
	man, _, err := pgbac.Export(ctx, src.ConnString, archive, pgbac.ExportOptions{Name: "x", Version: "1", Generator: "test"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if man.Kind != pgbac.Kind || man.Data == nil || len(man.Data.Tables) != 3 {
		t.Fatalf("manifest: kind=%q tables=%v", man.Kind, man.Data)
	}
	// A generated column is computed on import, so its values are not copied;
	// before PG12 upper_name is a plain column and is.
	wantColumns := "id,name"
	if devdb.DefaultMajor() < 12 {
		wantColumns = "id,name,upper_name"
	}
	for _, td := range man.Data.Tables {
		if td.Name == "parent" && (td.Rows != 3 || strings.Join(td.Columns, ",") != wantColumns) {
			t.Errorf("parent data: %+v (want columns %s)", td, wantColumns)
		}
	}

	rep, err := pgbac.Import(ctx, dst.ConnString, archive, pgbac.ImportOptions{Generator: "test"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rep.Tables != 3 || rep.Rows != 6 {
		t.Fatalf("report: %+v", rep)
	}

	b, err := pgx.Connect(ctx, dst.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	var names, notes, uppers string
	if err := b.QueryRow(ctx, `SELECT string_agg(name, '|' ORDER BY id), string_agg(upper_name, '|' ORDER BY id) FROM app.parent`).Scan(&names, &uppers); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(names, "a|b|c, with \"quotes\"\nand a newline") || !strings.HasPrefix(uppers, "A|B|C, WITH") {
		t.Fatalf("parent rows wrong: %q / %q", names, uppers)
	}
	if err := b.QueryRow(ctx, `SELECT string_agg(coalesce(note, '<null>'), '|' ORDER BY id) FROM app.child`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	// The source trigger ran once at insert time ('!' appended, NULL became
	// '!'); the import must copy those values verbatim, not fire it again.
	if notes != "x!|!|z!" {
		t.Fatalf("child rows must be loaded without firing the trigger again: %q", notes)
	}
	var pid, cid int
	if err := b.QueryRow(ctx, `INSERT INTO app.parent(name) VALUES ('d') RETURNING id`).Scan(&pid); err != nil || pid != 4 {
		t.Fatalf("identity must continue at 4: %d %v", pid, err)
	}
	if err := b.QueryRow(ctx, `INSERT INTO app.child(parent_id) VALUES (4) RETURNING id`).Scan(&cid); err != nil || cid != 4 {
		t.Fatalf("serial must continue at 4: %d %v", cid, err)
	}
	if _, err := b.Exec(ctx, `INSERT INTO app.child(parent_id) VALUES (99)`); err == nil {
		t.Fatal("foreign key must be enforced after import")
	}
	var mvN int
	if err := b.QueryRow(ctx, `SELECT n FROM app.mv`).Scan(&mvN); err != nil || mvN != 3 {
		t.Fatalf("materialized view must be populated with the imported rows: %d %v", mvN, err)
	}
	var canRead bool
	if err := b.QueryRow(ctx, `SELECT has_table_privilege('bac_reader', 'app.parent', 'SELECT')`).Scan(&canRead); err != nil || !canRead {
		t.Fatalf("grant must come across: %v %v", canRead, err)
	}
	var baselines int
	if err := b.QueryRow(ctx, `SELECT count(*) FROM pgdac.baseline`).Scan(&baselines); err != nil || baselines != 1 {
		t.Fatalf("import must record a baseline: %d %v", baselines, err)
	}

	// Schemas equal.
	rawA, _ := introspect.Introspect(ctx, a, introspect.Options{})
	rawB, _ := introspect.Introspect(ctx, b, introspect.Options{})
	ma, _ := build.Canonicalize(ctx, rawA, build.Options{})
	mb, _ := build.Canonicalize(ctx, rawB, build.Options{})
	if res, _ := engine.Compare(mb, ma, engine.DefaultOptions()); !res.Empty() {
		t.Fatalf("imported schema differs:\n%s", res.Script(true))
	}

	// A second import into the now-populated database is refused.
	if _, err := pgbac.Import(ctx, dst.ConnString, archive, pgbac.ImportOptions{}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("import into a non-empty database must be refused: %v", err)
	}

	// Tampering with a data part is detected before anything is loaded.
	tampered := filepath.Join(t.TempDir(), "t.pgbac")
	corruptDataPart(t, archive, tampered)
	third, err := devdb.Acquire(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Release()
	if _, err := pgbac.Import(ctx, third.ConnString, tampered, pgbac.ImportOptions{}); err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("a tampered archive must be refused: %v", err)
	}
}

// corruptDataPart rewrites the archive with one data part's bytes flipped
// (the manifest unchanged), so part digests no longer match.
func corruptDataPart(t *testing.T, from, to string) {
	t.Helper()
	zr, err := zip.OpenReader(from)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(zf.Name, pgbac.DataDir) && len(data) > 20 {
			data[len(data)-5] ^= 0xff
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: zf.Name, Method: zf.Method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// parentTable and parentFill give app.parent a generated column where the
// scratch major has them (PG12+): export must leave it out of the copied
// rows. Before PG12 upper_name is a plain column the fixture fills, so the
// rows still round-trip.
func parentTable() string {
	if devdb.DefaultMajor() >= 12 {
		return `CREATE TABLE app.parent(id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL, upper_name text GENERATED ALWAYS AS (upper(name)) STORED);`
	}
	return `CREATE TABLE app.parent(id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL, upper_name text);`
}

func parentFill() string {
	if devdb.DefaultMajor() >= 12 {
		return ""
	}
	return `UPDATE app.parent SET upper_name = upper(name);`
}
