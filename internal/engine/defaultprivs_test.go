package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

func dpEntry(role, schema, objtype string, grants ...model.Grant) *model.DefaultPrivilege {
	return &model.DefaultPrivilege{Role: role, Schema: schema, ObjectType: objtype, Grants: grants}
}

func selectFor(grantee string) model.Grant {
	return model.Grant{Grantee: grantee, Privileges: []model.Privilege{{Type: "SELECT"}}}
}

func dpScript(t *testing.T, from, to *model.Database, opts Options) string {
	t.Helper()
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res.Script(true)
}

// Database-wide entries of a role the desired state does not name belong to
// someone else (another application sharing the database) and are left alone,
// even under the drop policy; naming the role makes them the project's.
func TestDefaultPrivilegesLeaveOtherRolesAlone(t *testing.T) {
	from := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "public"}},
		DefaultPrivileges: []*model.DefaultPrivilege{dpEntry("other_app", "", "tables", selectFor("readers"))}}
	to := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "public"}}}
	opts := DefaultOptions()
	opts.DropObjectsNotInSource = true

	if got := dpScript(t, from, to, opts); strings.Contains(got, "DEFAULT PRIVILEGES") {
		t.Fatalf("another role's database-wide entry was changed:\n%s", got)
	}
	opts.DefaultPrivilegeRoles = []string{"other_app"}
	if got := dpScript(t, from, to, opts); !strings.Contains(got, `ALTER DEFAULT PRIVILEGES FOR ROLE "other_app" REVOKE SELECT ON TABLES FROM "readers";`) {
		t.Fatalf("a role the project names is not managed:\n%s", got)
	}
}

// An entry the desired state has is managed in full: its revokes apply under
// any drop policy. Privileges the target adds without the desired state having
// the entry are revoked only under the drop policy.
func TestDefaultPrivilegesRevokeRules(t *testing.T) {
	app := []*model.Schema{{Name: "app"}}
	from := &model.Database{TargetVersion: 160000, Schemas: app, DefaultPrivileges: []*model.DefaultPrivilege{
		dpEntry("owner", "app", "tables", selectFor("readers"), selectFor("writers")),
		dpEntry("owner", "app", "sequences", model.Grant{Grantee: "readers", Privileges: []model.Privilege{{Type: "USAGE"}}}),
	}}
	to := &model.Database{TargetVersion: 160000, Schemas: app, DefaultPrivileges: []*model.DefaultPrivilege{
		dpEntry("owner", "app", "tables", selectFor("readers")),
	}}
	opts := DefaultOptions()
	got := dpScript(t, from, to, opts)
	if !strings.Contains(got, `REVOKE SELECT ON TABLES FROM "writers"`) {
		t.Errorf("a declared entry's revoke was not applied:\n%s", got)
	}
	if strings.Contains(got, "ON SEQUENCES") {
		t.Errorf("an entry the desired state lacks was revoked without the drop policy:\n%s", got)
	}
	opts.DropObjectsNotInSource = true
	if got := dpScript(t, from, to, opts); !strings.Contains(got, `REVOKE USAGE ON SEQUENCES FROM "readers"`) {
		t.Errorf("the drop policy did not revoke the undeclared entry:\n%s", got)
	}
	opts.IgnorePrivileges = true
	if got := dpScript(t, from, to, opts); strings.Contains(got, "DEFAULT PRIVILEGES") {
		t.Errorf("IgnorePrivileges compared default privileges:\n%s", got)
	}
}

// A schema the migration drops takes its entries with it, and a role the
// target lacks is a blocking hazard, as for object grants.
func TestDefaultPrivilegesDroppedSchemaAndMissingRole(t *testing.T) {
	from := &model.Database{TargetVersion: 160000, Roles: []string{"owner"},
		Schemas:           []*model.Schema{{Name: "public"}, {Name: "old"}},
		DefaultPrivileges: []*model.DefaultPrivilege{dpEntry("owner", "old", "tables", selectFor("readers"))}}
	to := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "public"}},
		DefaultPrivileges: []*model.DefaultPrivilege{dpEntry("owner", "public", "tables", selectFor("readers"))}}
	opts := DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Script(true)
	if strings.Contains(got, `IN SCHEMA "old"`) {
		t.Errorf("revoked default privileges of a schema being dropped:\n%s", got)
	}
	if !strings.Contains(got, `IN SCHEMA "public" GRANT SELECT ON TABLES TO "readers"`) || !res.HasBlocking() {
		t.Errorf("a grant to a role the target lacks must carry a blocking hazard:\n%s", got)
	}
}
