package introspect_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
)

const fixtureSQL = `
CREATE SCHEMA app;
CREATE SEQUENCE app.s;
CREATE TYPE app.status AS ENUM ('a');
CREATE TABLE app.t(id int PRIMARY KEY DEFAULT nextval('app.s'), st app.status, CONSTRAINT t_chk CHECK (id > 0));
CREATE TABLE app.c(tid int);
ALTER TABLE app.c ADD CONSTRAINT c_fk FOREIGN KEY (tid) REFERENCES app.t(id) NOT VALID;
CREATE VIEW app.v AS SELECT id FROM app.t;
CREATE FUNCTION app.f(x app.status) RETURNS SETOF app.t LANGUAGE sql AS 'SELECT * FROM app.t';
CREATE FUNCTION app.one() RETURNS int LANGUAGE sql AS 'SELECT 1';
CREATE VIEW app.v2 AS SELECT app.one() AS o;
CREATE FUNCTION app.tf() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$;
CREATE TRIGGER tg BEFORE INSERT ON app.t FOR EACH ROW EXECUTE FUNCTION app.tf();
CREATE INDEX t_idx ON app.t ((id + 1));
CREATE POLICY p ON app.t USING (id IN (SELECT tid FROM app.c));
ALTER TABLE app.t DISABLE TRIGGER tg;
CREATE VIEW app.v3 WITH (security_barrier = true) AS SELECT id FROM app.t WHERE id > 0 WITH CHECK OPTION;
CREATE DOMAIN app.dom AS text NOT NULL DEFAULT 'x' CHECK (length(VALUE) > 0);
CREATE TYPE app.pt AS (x integer, y integer);
CREATE TABLE app.p (id int, d date) PARTITION BY RANGE (d);
CREATE TABLE app.p1 PARTITION OF app.p FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
CREATE UNLOGGED TABLE app.u (k text) WITH (fillfactor = 70);
ALTER TABLE app.u ALTER COLUMN k SET STORAGE EXTERNAL;
COMMENT ON COLUMN app.t.id IS 'pk';
`

func schema(db *model.Database, name string) *model.Schema {
	for _, s := range db.Schemas {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// TestIntrospect_Invariants exercises, against the embedded PostgreSQL, the
// properties the differ relies on: the model does not depend on the session's
// search_path (definitions are fully qualified), NOT VALID is carried by the
// Validated flag rather than the definition text, and dependency edges are
// recorded for functions (row types, enums), triggers (their function) and
// views (functions they call).
func TestIntrospect_Invariants(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
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
	if _, err := conn.Exec(ctx, fixtureSQL); err != nil {
		t.Fatal(err)
	}

	m1, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("search_path invariant", func(t *testing.T) {
		if _, err := conn.Exec(ctx, "SET search_path = app, public"); err != nil {
			t.Fatal(err)
		}
		m2, err := introspect.Introspect(ctx, conn, introspect.Options{})
		if err != nil {
			t.Fatal(err)
		}
		j1, _ := m1.Marshal()
		j2, _ := m2.Marshal()
		if !bytes.Equal(j1, j2) {
			t.Errorf("model differs with search_path=app,public:\n%s", diffLines(string(j1), string(j2)))
		}
		// The pin must be undone afterwards: the connection keeps its own search_path.
		var sp string
		if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&sp); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sp, "app") {
			t.Errorf("search_path was not restored after introspection: %q", sp)
		}
	})

	app := schema(m1, "app")
	if app == nil {
		t.Fatal("no app schema in model")
	}

	t.Run("definitions are qualified", func(t *testing.T) {
		tbl := m1.Table("app", "t")
		if got := tbl.Column("id").Default; !strings.Contains(got, "app.s") {
			t.Errorf("default should be schema-qualified, got %q", got)
		}
		if got := tbl.Column("st").Type; got != "app.status" {
			t.Errorf("column type should be schema-qualified, got %q", got)
		}
		for _, v := range app.Views {
			if v.Name == "v" && !strings.Contains(v.Definition, "app.t") {
				t.Errorf("view def should reference app.t, got %q", v.Definition)
			}
		}
		for _, f := range app.Functions {
			if f.Name == "f" && (!strings.Contains(f.Definition, "SETOF app.t") || f.IdentityArgs != "x app.status") {
				t.Errorf("function signature should be qualified, got args %q def %q", f.IdentityArgs, f.Definition)
			}
		}
	})

	t.Run("not valid", func(t *testing.T) {
		c := m1.Table("app", "c")
		var fk *model.Constraint
		for _, con := range c.Constraints {
			if con.Name == "c_fk" {
				fk = con
			}
		}
		if fk == nil {
			t.Fatal("c_fk not found")
		}
		if fk.Validated {
			t.Error("c_fk should be unvalidated")
		}
		if strings.Contains(fk.Definition, "NOT VALID") {
			t.Errorf("NOT VALID must not be part of the definition text: %q", fk.Definition)
		}
	})

	t.Run("dependencies", func(t *testing.T) {
		var fdeps []model.ObjRef
		for _, f := range app.Functions {
			if f.Name == "f" {
				fdeps = f.DependsOn
			}
		}
		wantTable := model.ObjRef{Kind: model.KindTable, Schema: "app", Name: "t"}
		wantEnum := model.ObjRef{Kind: model.KindEnum, Schema: "app", Name: "status"}
		if !contains(fdeps, wantTable) || !contains(fdeps, wantEnum) {
			t.Errorf("function f should depend on table app.t and enum app.status, got %v", fdeps)
		}

		tbl := m1.Table("app", "t")
		if len(tbl.Triggers) != 1 || !contains(tbl.Triggers[0].DependsOn, model.ObjRef{Kind: model.KindFunction, Schema: "app", Name: "tf()"}) {
			t.Errorf("trigger should depend on app.tf(), got %+v", tbl.Triggers)
		}

		for _, v := range app.Views {
			if v.Name == "v2" && !contains(v.DependsOn, model.ObjRef{Kind: model.KindFunction, Schema: "app", Name: "one()"}) {
				t.Errorf("view v2 should depend on function app.one(), got %v", v.DependsOn)
			}
			if v.Name == "v" {
				if len(v.Columns) != 1 || v.Columns[0] != (model.ViewColumn{Name: "id", Type: "integer"}) {
					t.Errorf("view v columns: got %+v", v.Columns)
				}
				if len(v.ColumnDeps) != 1 || v.ColumnDeps[0] != (model.ColumnRef{Schema: "app", Table: "t", Column: "id"}) {
					t.Errorf("view v column deps: got %+v", v.ColumnDeps)
				}
			}
		}
		for _, f := range app.Functions {
			if f.Name == "f" && (f.ReturnType != "SETOF app.t" || f.Arguments != "x app.status") {
				t.Errorf("function f: return %q arguments %q", f.ReturnType, f.Arguments)
			}
		}
	})

	t.Run("wp3 attributes", func(t *testing.T) {
		tbl := m1.Table("app", "t")
		if len(tbl.Triggers) != 1 || tbl.Triggers[0].Enabled != "disabled" {
			t.Errorf("trigger state: %+v", tbl.Triggers)
		}
		if tbl.Column("id").Comment != "pk" {
			t.Errorf("column comment: %q", tbl.Column("id").Comment)
		}
		for _, v := range app.Views {
			if v.Name == "v3" && (v.CheckOption != "cascaded" || !v.SecurityBarrier) {
				t.Errorf("view options: check=%q barrier=%v", v.CheckOption, v.SecurityBarrier)
			}
		}
		if len(app.Domains) != 1 || !strings.Contains(app.Domains[0].Definition, "NOT NULL") ||
			!strings.Contains(app.Domains[0].Definition, "DEFAULT 'x'::text") || !strings.Contains(app.Domains[0].Definition, "CHECK") {
			t.Errorf("domain: %+v", app.Domains)
		}
		if len(app.Composites) != 1 || app.Composites[0].Definition != `CREATE TYPE "app"."pt" AS (x integer, y integer)` {
			t.Errorf("composite: %+v", app.Composites)
		}
		p := m1.Table("app", "p")
		if p.PartitionKey != "RANGE (d)" || p.PartitionStrategy != "range" {
			t.Errorf("partitioned parent: key %q strategy %q", p.PartitionKey, p.PartitionStrategy)
		}
		p1 := m1.Table("app", "p1")
		if p1.PartitionOf == nil || p1.PartitionOf.Table != "p" || p1.PartitionOf.Bound != "FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')" || len(p1.Columns) != 0 {
			t.Errorf("partition: %+v cols=%d", p1.PartitionOf, len(p1.Columns))
		}
		u := m1.Table("app", "u")
		if u.Persistence != "unlogged" || u.RelOptions["fillfactor"] != "70" || u.Column("k").Storage != "external" {
			t.Errorf("table attrs: persistence=%q reloptions=%v storage=%q", u.Persistence, u.RelOptions, u.Column("k").Storage)
		}
	})
}

func contains(refs []model.ObjRef, want model.ObjRef) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}

func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, "- "+x, "+ "+y)
			if len(out) > 40 {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
