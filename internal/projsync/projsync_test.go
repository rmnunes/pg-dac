package projsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/project"
)

func writeProject(t *testing.T, files map[string]string) (string, *project.Manifest) {
	t.Helper()
	dir := t.TempDir()
	for rel, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	man := &project.Manifest{Name: "p", Version: "1", TargetMajor: 16, Include: []string{"**/*.sql"}, Roles: []string{"existing_role"}}
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}
	return dir, man
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// table app.t(a integer, b text) with a grant; a view app.v on it.
func dbModel(withB bool, grantee string) *model.Database {
	cols := []*model.Column{{Name: "a", Type: "integer"}}
	if withB {
		cols = append(cols, &model.Column{Name: "b", Type: "text"})
	}
	tbl := &model.Table{Schema: "app", Name: "t", Columns: cols}
	if grantee != "" {
		tbl.Grants = []model.Grant{{Grantee: grantee, Privileges: []model.Privilege{{Type: "SELECT"}}}}
	}
	v := &model.View{Schema: "app", Name: "v", Definition: "SELECT a FROM app.t", DependsOn: []model.ObjRef{tbl.Ref()},
		Columns: []model.ViewColumn{{Name: "a", Type: "integer"}}}
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{tbl}, Views: []*model.View{v}}}}
}

func compare(t *testing.T, from, to *model.Database) *engine.Result {
	t.Helper()
	opts := engine.DefaultOptions()
	res, err := engine.Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A declarative file keeps its header comment and untouched statements; the
// changed object's statements are replaced in place by the canonical rendering.
func TestUpdateReplacesDeclarativeDefinitionInPlace(t *testing.T) {
	dir, man := writeProject(t, map[string]string{
		"schema/app/tables/t.sql": "-- Header: the t table\nCREATE TABLE app.t (\n    a integer\n);\n\n-- a note on the index\nCREATE INDEX t_a ON app.t (a);\n",
		"schema/app/views/v.sql":  "CREATE VIEW app.v AS SELECT a FROM app.t;\n",
	})
	from := dbModel(false, "")
	from.Schemas[0].Tables[0].Indexes = []*model.Index{{Name: "t_a", Table: "t", Definition: `CREATE INDEX t_a ON app.t USING btree (a)`}}
	to := dbModel(true, "reader")
	to.Schemas[0].Tables[0].Indexes = from.Schemas[0].Tables[0].Indexes

	idx, err := Scan(dir, man, from)
	if err != nil {
		t.Fatal(err)
	}
	plan := Build(idx, man, from, to, compare(t, from, to))
	if len(plan.Refused) != 0 {
		t.Fatalf("unexpected refusals: %+v", plan.Refused)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != "schema/app/tables/t.sql" || plan.Changes[0].Action != "modify" {
		t.Fatalf("want one modify of the table file, got %+v", plan.Changes)
	}
	if !strings.Contains(plan.Diff(), `+    "b" text`) {
		t.Fatalf("diff should show the added column:\n%s", plan.Diff())
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	got := read(t, dir, "schema/app/tables/t.sql")
	for _, want := range []string{"-- Header: the t table\n", `CREATE TABLE "app"."t"`, `"b" text`, `CREATE INDEX t_a ON app.t USING btree (a);`, `GRANT SELECT ON TABLE "app"."t" TO "reader";`} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten file lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "CREATE INDEX") != 1 || strings.Contains(got, "-- a note on the index") {
		t.Errorf("the index statement is part of the table's rendering; its old copy must go:\n%s", got)
	}
	if read(t, dir, "schema/app/views/v.sql") != "CREATE VIEW app.v AS SELECT a FROM app.t;\n" {
		t.Error("an untouched object's file must not change")
	}
	if man2, _ := project.Load(dir); len(man2.Roles) != 2 || man2.Roles[1] != "reader" {
		t.Errorf("roles should gain the new grantee: %v", man2.Roles)
	}
}

// New objects get files at the stable paths; dropped objects lose theirs when
// the policy drops them, and keep them otherwise.
func TestUpdateCreatesAndDeletesFiles(t *testing.T) {
	dir, man := writeProject(t, map[string]string{
		"schema/app/views/v.sql":  "CREATE VIEW app.v AS SELECT a FROM app.t;\n",
		"schema/app/tables/t.sql": "CREATE TABLE app.t (a integer);\n",
	})
	from := dbModel(false, "")
	to := dbModel(false, "")
	to.Schemas[0].Views = nil // the database no longer has the view
	to.Schemas[0].Sequences = []*model.Sequence{{Schema: "app", Name: "s", Increment: 1}}

	idx, err := Scan(dir, man, from)
	if err != nil {
		t.Fatal(err)
	}
	plan := Build(idx, man, from, to, compare(t, from, to))
	paths := map[string]string{}
	for _, c := range plan.Changes {
		paths[c.Path] = c.Action
	}
	if paths["schema/app/sequences/s.sql"] != "create" {
		t.Errorf("new sequence should get a file: %+v", plan.Changes)
	}
	if _, dropped := paths["schema/app/views/v.sql"]; dropped {
		t.Errorf("default policy keeps the view the database lacks: %+v", plan.Changes)
	}

	opts := engine.DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, _ := engine.Compare(from, to, opts)
	plan = Build(idx, man, from, to, res)
	paths = map[string]string{}
	for _, c := range plan.Changes {
		paths[c.Path] = c.Action
	}
	if paths["schema/app/views/v.sql"] != "delete" {
		t.Errorf("drop policy all removes the view's file: %+v", plan.Changes)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "schema/app/views/v.sql") != "<missing>" || !strings.Contains(read(t, dir, "schema/app/sequences/s.sql"), "CREATE SEQUENCE") {
		t.Error("files not applied as planned")
	}
}

// An object defined by migration steps cannot be rewritten: the update refuses
// it with directions and still writes everything else.
func TestUpdateRefusesImperativeDefinitions(t *testing.T) {
	dir, man := writeProject(t, map[string]string{
		"deploy/01_t.sql": "CREATE TABLE IF NOT EXISTS app.t (a integer);\nALTER TABLE app.t ADD COLUMN IF NOT EXISTS legacy text;\n",
		"deploy/02_v.sql": "CREATE VIEW app.v AS SELECT a FROM app.t;\n",
	})
	from := dbModel(false, "")
	to := dbModel(true, "")
	to.Schemas[0].Views[0].Definition = "SELECT a, b FROM app.t"
	to.Schemas[0].Views[0].Columns = append(to.Schemas[0].Views[0].Columns, model.ViewColumn{Name: "b", Type: "text"})

	idx, err := Scan(dir, man, from)
	if err != nil {
		t.Fatal(err)
	}
	plan := Build(idx, man, from, to, compare(t, from, to))
	if len(plan.Refused) != 1 || !strings.Contains(plan.Refused[0].Object, `"app"."t"`) || !strings.Contains(plan.Refused[0].Reason, "deploy/01_t.sql") {
		t.Fatalf("the imperative table must be refused with its file named: %+v", plan.Refused)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != "deploy/02_v.sql" {
		t.Fatalf("the declarative view is still updated: %+v", plan.Changes)
	}
}

// Overloaded routines form one unit: all overloads are rewritten together.
func TestUpdateRoutineOverloads(t *testing.T) {
	dir, man := writeProject(t, map[string]string{
		"schema/app/functions/f.sql": "CREATE FUNCTION app.f(a int) RETURNS int LANGUAGE sql AS 'SELECT a';\nCREATE FUNCTION app.f(a text) RETURNS text LANGUAGE sql AS 'SELECT a';\n",
	})
	mk := func(body string) *model.Database {
		f1 := &model.Function{Schema: "app", Name: "f", Kind: "function", IdentityArgs: "integer", Language: "sql", ReturnType: "integer",
			Definition: "CREATE OR REPLACE FUNCTION app.f(a integer)\n RETURNS integer\n LANGUAGE sql\nAS $function$" + body + "$function$"}
		f2 := &model.Function{Schema: "app", Name: "f", Kind: "function", IdentityArgs: "text", Language: "sql", ReturnType: "text",
			Definition: "CREATE OR REPLACE FUNCTION app.f(a text)\n RETURNS text\n LANGUAGE sql\nAS $function$SELECT a$function$"}
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Functions: []*model.Function{f1, f2}}}}
	}
	from, to := mk("SELECT a"), mk("SELECT a + 1")
	idx, err := Scan(dir, man, from)
	if err != nil {
		t.Fatal(err)
	}
	plan := Build(idx, man, from, to, compare(t, from, to))
	if len(plan.Changes) != 1 {
		t.Fatalf("want one modified file, got %+v %+v", plan.Changes, plan.Refused)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	got := read(t, dir, "schema/app/functions/f.sql")
	if strings.Count(got, "CREATE OR REPLACE FUNCTION app.f(") != 2 || !strings.Contains(got, "SELECT a + 1") {
		t.Fatalf("both overloads must be present once, with the new body:\n%s", got)
	}
}

func TestUnifiedDiff(t *testing.T) {
	d := unifiedDiff("a\nb\nc\nd\ne\nf\ng\n", "a\nb\nC\nd\ne\nf\ng\n")
	if !strings.Contains(d, "-c\n+C\n") || !strings.Contains(d, "@@ -1,5 +1,5 @@") {
		t.Fatalf("unexpected diff:\n%s", d)
	}
	if unifiedDiff("same\n", "same\n") != "" {
		t.Fatal("identical texts have an empty diff")
	}
}
