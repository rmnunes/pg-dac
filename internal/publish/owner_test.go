package publish_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/publish"
	"github.com/rmnunes/pg-dac/internal/source"
)

// A project with an owner, published by a deploying role that is not the
// owner, onto a target whose objects that deploying role owns: the publish
// refuses until the deploying role may act as the owner, blocks on the
// reassignment until it is allowed, then gives every object to the owner,
// creates the new ones as the owner (so its default privileges apply), and
// leaves nothing to do. The owner is an ordinary role here: the desired state
// is built first (the scratch build makes its owner a superuser in the
// cluster it uses, which in this test is also the target's), and published
// from the artifact once that is undone.
func TestRun_ProjectOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
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
	exec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, r := range []string{"po_owner", "po_reader", "po_deployer"} {
		exec(`DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='` + r + `') THEN CREATE ROLE ` + r + ` NOLOGIN; END IF; END$$;`)
	}
	// The target as an earlier, owner-less deployment left it: everything
	// belongs to the deploying role.
	exec(`CREATE SCHEMA app AUTHORIZATION po_deployer;
CREATE TABLE app.t(a int); ALTER TABLE app.t OWNER TO po_deployer;
CREATE FUNCTION app.f() RETURNS int LANGUAGE sql AS 'SELECT 1'; ALTER FUNCTION app.f() OWNER TO po_deployer;`)

	// The project: owned by po_owner, whose default privileges give readers
	// every table of app; f is not for PUBLIC.
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pgdac.yaml", "name: owned\nversion: 1.0.0\ntargetMajor: 0\nowner: po_owner\nroles: [po_reader]\nschemas: [app]\ninclude: [\"schema/**/*.sql\"]\n")
	write("schema/app.sql", `CREATE SCHEMA app;
ALTER DEFAULT PRIVILEGES IN SCHEMA app GRANT SELECT ON TABLES TO po_reader;
CREATE TABLE app.t(a int, b text);
CREATE TABLE app.n(id int PRIMARY KEY);
CREATE FUNCTION app.f() RETURNS int LANGUAGE sql AS 'SELECT 1';
REVOKE EXECUTE ON FUNCTION app.f() FROM PUBLIC;`)

	srcOpts := source.Options{Schemas: []string{"app"}}
	desired, _, err := source.Resolve(ctx, dir, srcOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range desired.OwnedObjects() {
		if o.Owner != "po_owner" {
			t.Fatalf("the build must create %s as the owner, got %q", o.Ref, o.Owner)
		}
	}
	dplan, err := deploy.ForSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "owned.pgdac")
	bo := artifact.BuildOptions{Name: "owned", Version: "1.0.0", Generator: "pg-dac test"}
	bo.Extra, bo.Deploy = dplan.ArtifactParts()
	if _, err := artifact.Build(pkg, desired, bo); err != nil {
		t.Fatal(err)
	}
	exec(`ALTER ROLE po_owner NOSUPERUSER`)

	plan, err := deploy.ForSource(pkg)
	if err != nil || plan.Owner != "po_owner" {
		t.Fatalf("the artifact must carry the owner: %+v err=%v", plan, err)
	}
	opts := engine.DefaultOptions()
	deploy.ApplyOwner(&opts, plan)
	req := publish.Request{Source: pkg, SourceOptions: srcOpts, Plan: plan, Engine: opts, Exec: execute.DefaultOptions()}

	// Publish as the deploying role.
	exec(`SET SESSION AUTHORIZATION po_deployer`)

	_, err = publish.Run(ctx, conn, req, publish.Observer{})
	var blocked *publish.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("giving the deploying role's objects to the owner must block: %v", err)
	}

	req.Engine.AllowHazards = true
	_, err = publish.Run(ctx, conn, req, publish.Observer{})
	if err == nil || !strings.Contains(err.Error(), `GRANT "po_owner" TO "po_deployer"`) {
		t.Fatalf("a deploying role that cannot act as the owner must be refused up front: %v", err)
	}
	var cols int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='app' AND table_name='t'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("the refusal must come before anything runs: columns=%d err=%v", cols, err)
	}

	exec(`RESET SESSION AUTHORIZATION`)
	exec(`GRANT po_owner TO po_deployer`)
	exec(`SET SESSION AUTHORIZATION po_deployer`)
	// ALTER SCHEMA … OWNER TO wants CREATE on the database from whoever runs it.
	_, err = publish.Run(ctx, conn, req, publish.Observer{})
	if err == nil || !strings.Contains(err.Error(), `needs CREATE on the database`) || !strings.Contains(err.Error(), `TO "po_deployer"`) {
		t.Fatalf("a deploying role without CREATE on the database must be refused up front: %v", err)
	}

	exec(`RESET SESSION AUTHORIZATION`)
	var dbname string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&dbname); err != nil {
		t.Fatal(err)
	}
	exec(`GRANT CREATE ON DATABASE ` + dbname + ` TO po_deployer`)
	exec(`SET SESSION AUTHORIZATION po_deployer`)
	res, err := publish.Run(ctx, conn, req, publish.Observer{})
	if err != nil {
		script := ""
		if res != nil && res.Migration != nil {
			script = res.Migration.Script(true)
		}
		t.Fatalf("publish: %v\n%s", err, script)
	}
	exec(`RESET SESSION AUTHORIZATION`)

	type row struct{ Obj, Owner string }
	rows, err := conn.Query(ctx, `
SELECT 'schema app', pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'app'
UNION ALL SELECT 'relation ' || relname, pg_get_userbyid(relowner) FROM pg_class WHERE relnamespace = 'app'::regnamespace AND relkind = 'r'
UNION ALL SELECT 'function ' || proname, pg_get_userbyid(proowner) FROM pg_proc WHERE pronamespace = 'app'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want the schema, two tables and the function: %v", got)
	}
	for _, r := range got {
		if r.Owner != "po_owner" {
			t.Errorf("%s is owned by %s, want po_owner", r.Obj, r.Owner)
		}
	}
	var readerN, readerT, publicF bool
	if err := conn.QueryRow(ctx, `SELECT has_table_privilege('po_reader', 'app.n', 'SELECT'),
       has_table_privilege('po_reader', 'app.t', 'SELECT'),
       has_function_privilege('public', 'app.f()', 'EXECUTE')`).Scan(&readerN, &readerT, &publicF); err != nil {
		t.Fatal(err)
	}
	if !readerN || !readerT || publicF {
		t.Errorf("reader SELECT on n=%v t=%v (want both), PUBLIC EXECUTE on f=%v (want false)", readerN, readerT, publicF)
	}
	var dp int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_default_acl WHERE defaclrole = 'po_owner'::regrole AND defaclnamespace = 'app'::regnamespace`).Scan(&dp); err != nil || dp != 1 {
		t.Errorf("the owner's default privileges in app: %d err=%v", dp, err)
	}

	// Nothing is left to do.
	after, err := introspect.Introspect(ctx, conn, introspect.Options{IncludeSchemas: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := source.Canonical(ctx, after, srcOpts)
	again, err := engine.Compare(canon, desired, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("the published target must match the project:\n%s", again.Script(true))
	}
}
