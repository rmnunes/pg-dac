package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

func sel(grantee string, grantable bool) model.Grant {
	return model.Grant{Grantee: grantee, Privileges: []model.Privilege{{Type: "SELECT", Grantable: grantable}}}
}

// privFixture: table app.t(a integer, b text) with the given grants, and a view
// app.v reading t.a (its grants given separately).
func privFixture(tableGrants, viewGrants []model.Grant, viewCols ...model.ViewColumn) *model.Database {
	t := &model.Table{Schema: "app", Name: "t", Grants: tableGrants,
		Columns: []*model.Column{{Name: "a", Type: "integer"}, {Name: "b", Type: "text"}}}
	if len(viewCols) == 0 {
		viewCols = []model.ViewColumn{{Name: "a", Type: "integer"}}
	}
	def := "SELECT a FROM app.t"
	if len(viewCols) == 2 {
		def = "SELECT a, b FROM app.t"
	}
	v := &model.View{Schema: "app", Name: "v", Definition: def, Grants: viewGrants,
		DependsOn:  []model.ObjRef{t.Ref()},
		ColumnDeps: []model.ColumnRef{{Schema: "app", Table: "t", Column: "a"}},
		Columns:    viewCols}
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{t}, Views: []*model.View{v}}}}
}

func indexOfOrFail(t *testing.T, script, needle string) int {
	t.Helper()
	i := strings.Index(script, needle)
	if i < 0 {
		t.Fatalf("expected %q in script:\n%s", needle, script)
	}
	return i
}

// A privilege the source holds and the target lacks is granted; one the target
// holds and the source lacks is revoked only under the drop policy that drops
// objects not in the source (SSDT's DropPermissionsNotInSource).
func TestPrivileges_GrantAndRevokeFollowPolicy(t *testing.T) {
	from := privFixture(nil, nil)
	to := privFixture([]model.Grant{sel("reader", false)}, nil)
	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	indexOfOrFail(t, res.Script(true), `GRANT SELECT ON TABLE "app"."t" TO "reader";`)

	// Reverse: the target has the grant, the source does not.
	res, err = Compare(to, from, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script(true), "REVOKE") {
		t.Fatalf("default drop policy must not revoke privileges the source does not mention:\n%s", res.Script(true))
	}
	opts := DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, err = Compare(to, from, opts)
	if err != nil {
		t.Fatal(err)
	}
	indexOfOrFail(t, res.Script(true), `REVOKE SELECT ON TABLE "app"."t" FROM "reader";`)
}

// PUBLIC is rendered as the keyword, grant options upgrade in place and
// downgrade with REVOKE GRANT OPTION FOR.
func TestPrivileges_PublicAndGrantOption(t *testing.T) {
	from := privFixture([]model.Grant{sel("", false), sel("reader", false)}, nil)
	to := privFixture([]model.Grant{sel("", false), sel("reader", true)}, nil)
	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	indexOfOrFail(t, res.Script(true), `GRANT SELECT ON TABLE "app"."t" TO "reader" WITH GRANT OPTION;`)

	opts := DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, err = Compare(to, from, opts)
	if err != nil {
		t.Fatal(err)
	}
	indexOfOrFail(t, res.Script(true), `REVOKE GRANT OPTION FOR SELECT ON TABLE "app"."t" FROM "reader";`)

	empty := privFixture(nil, nil)
	res, err = Compare(empty, privFixture([]model.Grant{sel("", false)}, nil), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	indexOfOrFail(t, res.Script(true), `GRANT SELECT ON TABLE "app"."t" TO PUBLIC;`)
}

// An object pg-dac drops and recreates gets its privileges back: the desired
// ones, plus — under a policy that would not revoke them — the ones the target
// held. With privileges ignored, the target's own grants are restored verbatim.
func TestPrivileges_RecreatedObjectKeepsGrants(t *testing.T) {
	two := []model.ViewColumn{{Name: "a", Type: "integer"}, {Name: "b", Type: "text"}}
	from := privFixture(nil, []model.Grant{sel("reader", false), sel("ops", false)}, two...)
	to := privFixture(nil, []model.Grant{sel("reader", false)}) // column dropped: view is recreated

	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	create := indexOfOrFail(t, script, `CREATE OR REPLACE VIEW "app"."v"`)
	reader := indexOfOrFail(t, script, `GRANT SELECT ON TABLE "app"."v" TO "reader";`)
	ops := indexOfOrFail(t, script, `GRANT SELECT ON TABLE "app"."v" TO "ops";`)
	if reader < create || ops < create {
		t.Fatalf("grants must follow the recreate:\n%s", script)
	}

	// Policy all: ops is not in the source and may be dropped — no re-grant.
	opts := DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, err = Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script(true), `TO "ops"`) {
		t.Fatalf("under drop policy all a privilege missing from the source must not be restored:\n%s", res.Script(true))
	}

	// Privileges ignored: the recreate is transparent — the target's grants come back.
	opts = DefaultOptions()
	opts.IgnorePrivileges = true
	res, err = Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script = res.Script(true)
	indexOfOrFail(t, script, `GRANT SELECT ON TABLE "app"."v" TO "ops";`)
	indexOfOrFail(t, script, `GRANT SELECT ON TABLE "app"."v" TO "reader";`)
	if self, _ := Compare(to, to, opts); !self.Empty() {
		t.Errorf("self-compare must stay empty:\n%s", self.Script(true))
	}
}

// Column privileges are modeled per column and granted after the column exists.
func TestPrivileges_ColumnGrants(t *testing.T) {
	from := privFixture(nil, nil)
	to := privFixture(nil, nil)
	to.Schemas[0].Tables[0].Columns[1].Grants = []model.Grant{{Grantee: "reader",
		Privileges: []model.Privilege{{Type: "SELECT"}, {Type: "UPDATE"}}}}
	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	indexOfOrFail(t, res.Script(true), `GRANT SELECT ("b"), UPDATE ("b") ON TABLE "app"."t" TO "reader";`)

	// A new table carries its column grants after its CREATE.
	res, err = Compare(&model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app"}}}, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	if indexOfOrFail(t, script, `GRANT SELECT ("b")`) < indexOfOrFail(t, script, `CREATE TABLE "app"."t"`) {
		t.Fatalf("column grant must follow CREATE TABLE:\n%s", script)
	}
}

// Granting to a role the live target does not have is a blocking hazard, so a
// publish stops before PostgreSQL fails half-way.
func TestPrivileges_MissingRoleBlocks(t *testing.T) {
	from := privFixture(nil, nil)
	from.Roles = []string{"postgres", "reader"}
	to := privFixture([]model.Grant{sel("reader", false), sel("nobody", false)}, nil)
	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasBlocking() {
		t.Fatalf("a grant to a missing role must block:\n%s", res.Script(true))
	}
	found := false
	for _, s := range res.Statements {
		for _, h := range s.Hazards {
			if h.Type == safety.MissingRole && strings.Contains(s.SQL, `"nobody"`) {
				found = true
			}
			if h.Type == safety.MissingRole && strings.Contains(s.SQL, `"reader"`) {
				t.Fatalf("existing role flagged as missing: %s", s.SQL)
			}
		}
	}
	if !found {
		t.Fatalf("MISSING_ROLE hazard not attached to the grant:\n%s", res.Script(true))
	}
	// Without a role list (an artifact or project as target) nothing is checked.
	from.Roles = nil
	res, _ = Compare(from, to, DefaultOptions())
	if res.HasBlocking() {
		t.Fatal("no role list: must not block")
	}
}

// Routine and schema privileges use their own object keywords.
func TestPrivileges_ObjectKeywords(t *testing.T) {
	f := &model.Function{Schema: "app", Name: "f", Kind: "function", IdentityArgs: "integer", Language: "sql",
		Definition: "CREATE OR REPLACE FUNCTION app.f(integer)\n RETURNS integer\n LANGUAGE sql\nAS $function$SELECT 1$function$"}
	p := &model.Function{Schema: "app", Name: "p", Kind: "procedure", Language: "sql",
		Definition: "CREATE OR REPLACE PROCEDURE app.p()\n LANGUAGE sql\nAS $procedure$SELECT 1$procedure$"}
	mk := func(withGrants bool) *model.Database {
		fc, pc := *f, *p
		s := &model.Schema{Name: "app", Functions: []*model.Function{&fc, &pc},
			Sequences: []*model.Sequence{{Schema: "app", Name: "s"}},
			Enums:     []*model.EnumType{{Schema: "app", Name: "e", Values: []string{"a"}}},
			Domains:   []*model.Domain{{Schema: "app", Name: "d", Definition: "CREATE DOMAIN \"app\".\"d\" AS integer"}}}
		if withGrants {
			exec := model.Grant{Grantee: "reader", Privileges: []model.Privilege{{Type: "EXECUTE"}}}
			usage := model.Grant{Grantee: "reader", Privileges: []model.Privilege{{Type: "USAGE"}}}
			fc.Grants, pc.Grants = []model.Grant{exec}, []model.Grant{exec}
			s.Grants = []model.Grant{usage}
			s.Sequences[0].Grants = []model.Grant{{Grantee: "reader", Privileges: []model.Privilege{{Type: "SELECT"}, {Type: "USAGE"}}}}
			s.Enums[0].Grants = []model.Grant{usage}
			s.Domains[0].Grants = []model.Grant{usage}
		}
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{s}}
	}
	res, err := Compare(mk(false), mk(true), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	for _, want := range []string{
		`GRANT EXECUTE ON FUNCTION "app"."f"(integer) TO "reader";`,
		`GRANT EXECUTE ON PROCEDURE "app"."p"() TO "reader";`,
		`GRANT USAGE ON SCHEMA "app" TO "reader";`,
		`GRANT SELECT, USAGE ON SEQUENCE "app"."s" TO "reader";`,
		`GRANT USAGE ON TYPE "app"."e" TO "reader";`,
		`GRANT USAGE ON DOMAIN "app"."d" TO "reader";`,
	} {
		indexOfOrFail(t, script, want)
	}
}
