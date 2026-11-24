package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/validate"
)

// Migrations of a project with an owner replay in a scratch database and
// converge: on a target the owner already owns (objects created as it, its
// default privileges applied) and on one whose objects another role owns
// (reassigned first). Builds run as the owner (build.Options.Owner), the way
// a project with owner: builds.
func TestOwnerMigrationsConverge(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	const roles = `DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='ol_reader') THEN CREATE ROLE ol_reader NOLOGIN; END IF; END$$;`
	desired := roles + `
CREATE SCHEMA app;
ALTER DEFAULT PRIVILEGES IN SCHEMA app GRANT SELECT ON TABLES TO ol_reader;
ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
CREATE TYPE app.mood AS ENUM ('ok', 'meh');
CREATE TABLE app.t(id serial PRIMARY KEY, m app.mood, note text);
CREATE VIEW app.v AS SELECT id, m FROM app.t;
CREATE FUNCTION app.f() RETURNS int LANGUAGE sql AS 'SELECT count(*)::int FROM app.t';
REVOKE SELECT ON app.v FROM ol_reader;`

	cases := []struct {
		name, from string
		fromOwner  string // "" builds the target as the scratch superuser: another owner
		reassigns  bool
	}{
		{"from-empty", roles, "ol_owner", false},
		{"owned-by-the-owner", roles + ` CREATE SCHEMA app; CREATE TABLE app.t(id serial PRIMARY KEY);`, "ol_owner", false},
		{"owned-by-another-role", roles + ` CREATE SCHEMA app; CREATE TABLE app.t(id serial PRIMARY KEY); CREATE FUNCTION app.f() RETURNS int LANGUAGE sql AS 'SELECT 1';`, "", true},
	}
	opts := engine.DefaultOptions()
	opts.Owner = "ol_owner"
	opts.AllowHazards = true
	opts.DropObjectsNotInSource = true

	to, _, err := build.FromSQL(ctx, desired, build.Options{Owner: "ol_owner"})
	if err != nil {
		t.Fatalf("build desired: %v", err)
	}
	for _, o := range to.OwnedObjects() {
		if o.Ref.Name != "public" && o.Owner != "ol_owner" {
			t.Fatalf("built as the owner, %s is owned by %q", o.Ref, o.Owner)
		}
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			from, _, err := build.FromSQL(ctx, c.from, build.Options{Owner: c.fromOwner})
			if err != nil {
				t.Fatalf("build target: %v", err)
			}
			res, err := engine.Compare(from, to, opts)
			if err != nil {
				t.Fatal(err)
			}
			script := res.Script(true)
			if got := strings.Contains(script, "OWNER TO"); got != c.reassigns {
				t.Fatalf("reassignments: got %v, want %v\n%s", got, c.reassigns, script)
			}
			vr, err := validate.Plan(ctx, from, to, opts)
			if err != nil {
				t.Fatalf("migration failed to apply: %v\n%s", err, script)
			}
			if !vr.Converged {
				t.Fatalf("migration did not converge; residual:\n%s\n--- script ---\n%s", vr.Residual.Script(true), script)
			}
		})
	}
}
