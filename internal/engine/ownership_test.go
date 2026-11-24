package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// ownerFixture: schema app (owned by owner) with table app.t(a integer), and
// the public schema owned by pg_database_owner. extra adds tables to app.
func ownerFixture(owner string, extra ...*model.Table) *model.Database {
	t := &model.Table{Schema: "app", Name: "t", Owner: owner, Columns: []*model.Column{{Name: "a", Type: "integer"}}}
	return &model.Database{TargetVersion: 160000, Roles: []string{"app_owner", "deployer", "reader"}, Schemas: []*model.Schema{
		{Name: "app", Owner: owner, Tables: append([]*model.Table{t}, extra...)},
		{Name: "public", Owner: "pg_database_owner"},
	}}
}

func ownerOptions() Options {
	opts := DefaultOptions()
	opts.Owner = "app_owner"
	return opts
}

// Objects the target has under another role are given to the project's owner
// first — the schema before what it holds — and each reassignment is a
// blocking hazard; the public schema keeps its owner.
func TestOwnership_ReassignsBeforeAnythingElse(t *testing.T) {
	from := ownerFixture("deployer")
	to := ownerFixture("app_owner")
	to.Schemas[0].Tables[0].Columns = append(to.Schemas[0].Tables[0].Columns, &model.Column{Name: "b", Type: "text"})

	res, err := Compare(from, to, ownerOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	ordered(t, script,
		`ALTER SCHEMA "app" OWNER TO "app_owner";`,
		`ALTER TABLE "app"."t" OWNER TO "app_owner";`,
		`SET ROLE "app_owner";`,
		`ADD COLUMN "b" text`,
		`RESET ROLE;`)
	if strings.Contains(script, `SCHEMA "public" OWNER`) {
		t.Errorf("the public schema must keep its owner:\n%s", script)
	}
	if !res.HasBlocking() {
		t.Fatal("a reassignment must block a publish until allowed")
	}
	n := 0
	for _, s := range res.Statements {
		for _, h := range s.Hazards {
			if h.Type == safety.ChangesOwner {
				n++
			}
		}
	}
	if n != 2 {
		t.Errorf("want a CHANGES_OWNER hazard on each of the 2 reassignments, got %d", n)
	}
	if res.Owner != "app_owner" {
		t.Errorf("Result.Owner = %q", res.Owner)
	}
	// The app shows the change of owner in the object's definitions.
	ocs, err := res.ObjectChanges()
	if err != nil {
		t.Fatal(err)
	}
	for _, oc := range ocs {
		if oc.ID == to.Schemas[0].Tables[0].Ref().Key() {
			if !strings.Contains(oc.OldDefinition, `OWNER TO "deployer";`) || !strings.Contains(oc.NewDefinition, `OWNER TO "app_owner";`) {
				t.Errorf("definitions must show the owners:\n%s\n---\n%s", oc.OldDefinition, oc.NewDefinition)
			}
		}
	}
}

// With IgnoreOwnership an object keeps the owner it has, and pg-dac changes
// it as the deploying role (it cannot act as that owner); what it creates is
// still created as the project's owner.
func TestOwnership_IgnoredKeepsOwnersAndActsForThem(t *testing.T) {
	from := ownerFixture("deployer")
	to := ownerFixture("app_owner", &model.Table{Schema: "app", Name: "n", Owner: "app_owner", Columns: []*model.Column{{Name: "id", Type: "integer"}}})
	to.Schemas[0].Tables[0].Columns = append(to.Schemas[0].Tables[0].Columns, &model.Column{Name: "b", Type: "text"})
	opts := ownerOptions()
	opts.IgnoreOwnership = true

	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	if strings.Contains(script, "OWNER TO") {
		t.Fatalf("ownership is ignored:\n%s", script)
	}
	add := indexOf(t, script, `ADD COLUMN "b" text`)
	set := indexOf(t, script, `SET ROLE "app_owner";`)
	create := indexOf(t, script, `CREATE TABLE "app"."n"`)
	if !(set < create) || (add > set && add < create) {
		t.Fatalf("the new table is created as the owner, the column added to the deployer's table as the deployer:\n%s", script)
	}
	if res.HasBlocking() {
		t.Fatalf("nothing should block:\n%s", script)
	}
}

// Extensions are created as the deploying role, between runs of statements
// made as the owner; the script ends as the role it started as.
func TestOwnership_ExtensionsRunAsTheDeployingRole(t *testing.T) {
	from := ownerFixture("app_owner")
	to := ownerFixture("app_owner", &model.Table{Schema: "app", Name: "n", Owner: "app_owner", Columns: []*model.Column{{Name: "id", Type: "integer"}}})
	to.Extensions = []*model.Extension{{Name: "pgcrypto", Schema: "public"}}

	res, err := Compare(from, to, ownerOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	ext := indexOf(t, script, `CREATE EXTENSION IF NOT EXISTS "pgcrypto"`)
	set := indexOf(t, script, `SET ROLE "app_owner";`)
	if ext > set {
		t.Fatalf("the extension must be created before switching to the owner:\n%s", script)
	}
	if !strings.HasSuffix(strings.TrimSpace(script), "RESET ROLE;") {
		t.Fatalf("the script must end as the deploying role:\n%s", script)
	}
}

// The owner's privileges on what it owns are implicit: a grant the target
// holds for the owner on an object being given to it is not revoked (the
// REVOKE would take away the owner's own rights once it owns the object).
func TestOwnership_OwnerPrivilegesAreImplicit(t *testing.T) {
	from := ownerFixture("deployer")
	from.Schemas[0].Tables[0].Grants = []model.Grant{sel("app_owner", false), sel("reader", false)}
	to := ownerFixture("app_owner")
	opts := ownerOptions()
	opts.DropObjectsNotInSource = true

	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	if strings.Contains(script, `FROM "app_owner"`) {
		t.Fatalf("the owner's own privileges must not be revoked:\n%s", script)
	}
	indexOfOrFail(t, script, `REVOKE SELECT ON TABLE "app"."t" FROM "reader";`)
}

// An owner the target lacks blocks the publish and is reported with the
// missing roles.
func TestOwnership_MissingOwnerBlocks(t *testing.T) {
	from := ownerFixture("deployer")
	from.Roles = []string{"deployer"}
	to := ownerFixture("app_owner", &model.Table{Schema: "app", Name: "n", Owner: "app_owner"})
	opts := ownerOptions()
	opts.IgnoreOwnership = true

	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasBlocking() || !res.OnlyMissingRoles() {
		t.Fatalf("a missing owner must block as a missing role:\n%s", res.Script(true))
	}
	if got := strings.Join(res.MissingRoles(), ","); got != "app_owner" {
		t.Fatalf("missing roles = %q", got)
	}
}

// Without an owner nothing changes: no role switches, no reassignment.
func TestOwnership_NoOwnerNoRoles(t *testing.T) {
	res, err := Compare(ownerFixture("deployer"), ownerFixture("someone"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty() {
		t.Fatalf("ownership is not compared without an owner:\n%s", res.Script(true))
	}
}

// A routine the migration creates is born with PUBLIC's EXECUTE; when the
// source does not hold it, it is revoked right after the CREATE, whatever the
// drop policy (it is the source's own state). Under IgnorePrivileges a new
// routine is left as PostgreSQL makes it.
func TestPrivileges_NewObjectLosesWhatItIsBornWith(t *testing.T) {
	fn := func(grants ...model.Grant) *model.Database {
		f := &model.Function{Schema: "app", Name: "f", Kind: "function", Language: "sql", ReturnType: "integer",
			Definition: "CREATE OR REPLACE FUNCTION app.f()\n RETURNS integer\n LANGUAGE sql\nAS $function$SELECT 1$function$\n",
			Grants:     grants}
		e := &model.EnumType{Schema: "app", Name: "e", Values: []string{"x"}}
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Functions: []*model.Function{f}, Enums: []*model.EnumType{e}}}}
	}
	from := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app"}}}
	exec := func(grantee string) model.Grant {
		return model.Grant{Grantee: grantee, Privileges: []model.Privilege{{Type: "EXECUTE"}}}
	}

	res, err := Compare(from, fn(exec("reader")), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	ordered(t, script, `CREATE OR REPLACE FUNCTION app.f()`, `REVOKE EXECUTE ON FUNCTION "app"."f"() FROM PUBLIC;`)
	indexOfOrFail(t, script, `REVOKE USAGE ON TYPE "app"."e" FROM PUBLIC;`)
	indexOfOrFail(t, script, `GRANT EXECUTE ON FUNCTION "app"."f"() TO "reader";`)

	// Holding PUBLIC's EXECUTE, there is nothing to revoke.
	res, _ = Compare(from, fn(exec("")), DefaultOptions())
	if strings.Contains(res.Script(true), `ON FUNCTION "app"."f"() FROM PUBLIC`) {
		t.Fatalf("PUBLIC keeps EXECUTE:\n%s", res.Script(true))
	}

	opts := DefaultOptions()
	opts.IgnorePrivileges = true
	res, _ = Compare(from, fn(), opts)
	if strings.Contains(res.Script(true), "REVOKE") {
		t.Fatalf("privileges are ignored:\n%s", res.Script(true))
	}
}

// Created as the owner, a table gets what the owner's default privileges
// give; what the source does not hold of that is revoked.
func TestPrivileges_NewObjectLosesWhatOwnerDefaultsGive(t *testing.T) {
	from := ownerFixture("app_owner")
	from.DefaultPrivileges = []*model.DefaultPrivilege{dpEntry("app_owner", "app", "tables", selectFor("reader"))}
	to := ownerFixture("app_owner", &model.Table{Schema: "app", Name: "n", Owner: "app_owner", Columns: []*model.Column{{Name: "id", Type: "integer"}}})
	to.DefaultPrivileges = from.DefaultPrivileges

	res, err := Compare(from, to, ownerOptions())
	if err != nil {
		t.Fatal(err)
	}
	ordered(t, res.Script(true), `CREATE TABLE "app"."n"`, `REVOKE SELECT ON TABLE "app"."n" FROM "reader";`)
}

// A routine that persists loses PUBLIC's EXECUTE when the source revoked it,
// whatever the drop policy: lacking a privilege every new routine is born
// with is the source's declared state, not a privilege the target added.
// What the target added (a grant to a role) still follows the drop policy.
func TestPrivileges_ExplicitRevokeOfBuiltinDefault(t *testing.T) {
	fn := func(grants ...model.Grant) *model.Database {
		f := &model.Function{Schema: "app", Name: "f", Kind: "function", Language: "sql", ReturnType: "integer",
			Definition: "CREATE OR REPLACE FUNCTION app.f()\n RETURNS integer\n LANGUAGE sql\nAS $function$SELECT 1$function$\n",
			Grants:     grants}
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Functions: []*model.Function{f}}}}
	}
	exec := func(grantee string) model.Grant {
		return model.Grant{Grantee: grantee, Privileges: []model.Privilege{{Type: "EXECUTE"}}}
	}
	res, err := Compare(fn(exec(""), exec("reader")), fn(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	indexOfOrFail(t, script, `REVOKE EXECUTE ON FUNCTION "app"."f"() FROM PUBLIC;`)
	if strings.Contains(script, `FROM "reader"`) {
		t.Fatalf("a grant the target added follows the drop policy:\n%s", script)
	}
}
