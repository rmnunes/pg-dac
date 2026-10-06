package validate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rodrigonunes/pg-dac/internal/artifact"
	"github.com/rodrigonunes/pg-dac/internal/build"
	"github.com/rodrigonunes/pg-dac/internal/devdb"
	"github.com/rodrigonunes/pg-dac/internal/engine"
	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/project"
	"github.com/rodrigonunes/pg-dac/internal/validate"
)

// bootstrapSQL is a deliberately awkward schema: serial AND identity columns,
// a SQL-language function body that queries a table, functions returning table
// and view row types, a view that calls a function that depends on another
// view, views on views, a trigger, RLS, a materialized view, an expression
// index, a custom collation, an extension type, a cross-schema sequence
// default, and a constraint kept NOT VALID. Each of these broke the from-empty
// bootstrap at some point; this fixture is the regression gate for all of them.
const bootstrapSQL = `
CREATE SCHEMA app;
CREATE SCHEMA ref;
CREATE EXTENSION IF NOT EXISTS citext;
CREATE TYPE app.status AS ENUM ('active', 'inactive');
CREATE COLLATION app.ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false);
CREATE SEQUENCE ref.global_seq START 100;
CREATE TABLE ref.regions (id serial PRIMARY KEY, code text NOT NULL UNIQUE);
CREATE TABLE app.users (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email citext NOT NULL,
  name text COLLATE app.ci,
  status app.status NOT NULL DEFAULT 'active',
  region_id int REFERENCES ref.regions(id),
  ext_ref bigint DEFAULT nextval('ref.global_seq'),
  CONSTRAINT users_email_uniq UNIQUE (email)
);
CREATE TABLE app.orders (
  id serial PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES app.users(id),
  amount numeric(10,2) NOT NULL CHECK (amount >= 0),
  kind character varying(2),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_user_idx ON app.orders (user_id);
CREATE INDEX orders_created_expr_idx ON app.orders ((date_trunc('day', (created_at AT TIME ZONE 'UTC'))));
-- IN (...) on a varchar deparses to a text that re-parses differently: the model
-- must be canonicalized to the fixed point or the bootstrap never converges.
CREATE INDEX orders_kind_idx ON app.orders (kind) WHERE kind IN ('F', 'FE');
CREATE VIEW app.final_orders AS SELECT id FROM app.orders WHERE kind IN ('F', 'FE');
CREATE FUNCTION app.order_count(p_user bigint) RETURNS bigint LANGUAGE sql STABLE
  AS 'SELECT count(*) FROM app.orders WHERE user_id = p_user';
CREATE FUNCTION app.active_users() RETURNS SETOF app.users LANGUAGE sql STABLE
  AS $$ SELECT * FROM app.users WHERE status = 'active' $$;
CREATE VIEW app.user_stats AS SELECT u.id, u.email, app.order_count(u.id) AS orders FROM app.users u;
CREATE VIEW app.busy_users AS SELECT * FROM app.user_stats WHERE orders > 10;
CREATE FUNCTION app.busy() RETURNS SETOF app.busy_users LANGUAGE sql STABLE AS 'SELECT * FROM app.busy_users';
CREATE VIEW app.busy_emails AS SELECT email FROM app.busy();
CREATE FUNCTION app.touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.created_at := now(); RETURN NEW; END $$;
CREATE TRIGGER orders_touch BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.touch();
ALTER TABLE app.orders ENABLE ROW LEVEL SECURITY;
CREATE POLICY orders_owner ON app.orders USING (user_id = current_setting('app.user_id', true)::bigint);
CREATE MATERIALIZED VIEW app.order_totals AS SELECT user_id, sum(amount) AS total FROM app.orders GROUP BY user_id;
ALTER TABLE app.orders ADD CONSTRAINT orders_amount_cap CHECK (amount < 1000000) NOT VALID;
COMMENT ON TABLE app.users IS 'Application users';
-- WP3: attributes that used to be dropped silently
CREATE DOMAIN app.email_addr AS text CHECK (VALUE LIKE '%@%');
CREATE TYPE app.point2 AS (x integer, y integer);
CREATE TABLE app.events (
  id bigserial,
  at timestamptz NOT NULL,
  contact app.email_addr,
  pos app.point2,
  PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);
CREATE TABLE app.events_2024 PARTITION OF app.events FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
CREATE TABLE app.events_default PARTITION OF app.events DEFAULT;
CREATE INDEX order_totals_user_idx ON app.order_totals (user_id);
CREATE UNLOGGED TABLE app.scratch (k text PRIMARY KEY, v text) WITH (fillfactor = 80);
ALTER TABLE app.scratch ALTER COLUMN v SET STORAGE EXTERNAL;
ALTER TABLE app.scratch ALTER COLUMN k SET STATISTICS 500;
ALTER TABLE app.orders DISABLE TRIGGER orders_touch;
CREATE VIEW app.open_orders AS SELECT id, amount FROM app.orders WHERE amount > 0 WITH CHECK OPTION;
CREATE VIEW app.secure_users WITH (security_barrier = true) AS SELECT id, email FROM app.users;
CREATE FUNCTION app.open_orders_ins() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO app.orders(user_id, amount) VALUES (1, NEW.amount); RETURN NEW; END $$;
CREATE TRIGGER open_orders_ins INSTEAD OF INSERT ON app.open_orders FOR EACH ROW EXECUTE FUNCTION app.open_orders_ins();
COMMENT ON COLUMN app.users.email IS 'login';
COMMENT ON INDEX app.orders_user_idx IS 'fk lookup';
COMMENT ON CONSTRAINT users_email_uniq ON app.users IS 'unique email';
`

// TestBootstrap_FromEmpty is the end-to-end gate for "publish an artifact into
// an empty database": build the model from SQL, generate the from-empty
// migration, replay it on a fresh scratch database, re-introspect, and require
// zero residual difference. It also requires the generated script to be
// byte-identical across repeated compares, and the model to survive a trip
// through a signed .pgdac.
func TestBootstrap_FromEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scratch-database test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	empty, _, err := build.FromSQL(ctx, "", build.Options{})
	if err != nil {
		t.Fatalf("build empty baseline: %v", err)
	}
	fixtures := map[string]string{"comprehensive": bootstrapSQL}
	if demo, _, err := build.FromDir(ctx, filepath.Join("..", "..", "testdata", "demo-project"), build.Options{}); err == nil {
		checkBootstrap(ctx, t, "demo-project", empty, demo)
	} else {
		t.Errorf("build testdata/demo-project: %v", err)
	}
	for name, sql := range fixtures {
		to, _, err := build.FromSQL(ctx, sql, build.Options{})
		if err != nil {
			t.Fatalf("%s: build: %v", name, err)
		}
		checkBootstrap(ctx, t, name, empty, to)
	}
}

// TestExtractProjectRoundTrip lays the comprehensive fixture out as an
// extracted project (engine.ObjectScripts: one numbered file per object in
// global dependency order), builds that project back through the scratch
// database, and requires it to compare empty against the original model in both
// directions. This is the gate for `extract --to-project` on schemas where a
// function's signature needs a view's row type or a view calls such a function.
func TestExtractProjectRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scratch-database test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	to, _, err := build.FromSQL(ctx, bootstrapSQL, build.Options{})
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	scripts := engine.ObjectScripts(to)
	if len(scripts) < 10 {
		t.Fatalf("expected many extracted scripts, got %d", len(scripts))
	}
	dir := t.TempDir()
	for _, sc := range scripts {
		if err := os.MkdirAll(filepath.Join(dir, sc.Section), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sc.Section, sc.FileName()), []byte(sc.SQL+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	man := project.Default("roundtrip")
	man.Include = []string{engine.ExtractSection + "/**/*.sql"}
	if mv := to.MajorVersion(); mv > 0 {
		man.TargetMajor = mv
	}
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}

	rebuilt, _, err := build.FromDir(ctx, dir, build.Options{})
	if err != nil {
		t.Fatalf("extracted project does not build: %v", err)
	}
	opts := engine.DefaultOptions()
	opts.DropObjectsNotInSource = true
	if res, err := engine.Compare(rebuilt, to, opts); err != nil || !res.Empty() {
		t.Errorf("extracted project differs from the original (err=%v):\n%s", err, res.Script(true))
	}
	if res, err := engine.Compare(to, rebuilt, opts); err != nil || !res.Empty() {
		t.Errorf("original differs from the extracted project (err=%v):\n%s", err, res.Script(true))
	}
}

func checkBootstrap(ctx context.Context, t *testing.T, name string, empty, to *model.Database) {
	t.Helper()
	opts := engine.DefaultOptions()

	// 1. The from-empty migration applies and converges.
	res, err := validate.Plan(ctx, empty, to, opts)
	if err != nil {
		mig, _ := engine.Compare(empty, to, opts)
		t.Fatalf("%s: bootstrap failed to apply: %v\n--- script ---\n%s", name, err, mig.Script(true))
	}
	if !res.Converged {
		t.Fatalf("%s: bootstrap did not converge; residual:\n%s", name, res.Residual.Script(true))
	}

	// 2. Self-compare of the built model is empty.
	if self, err := engine.Compare(to, to, opts); err != nil || !self.Empty() {
		t.Errorf("%s: self-compare should be empty (err=%v):\n%s", name, err, self.Script(true))
	}

	// 3. Deterministic output.
	first, _ := engine.Compare(empty, to, opts)
	for i := 0; i < 30; i++ {
		again, _ := engine.Compare(empty, to, opts)
		if again.Script(true) != first.Script(true) {
			t.Fatalf("%s: compare %d produced a different script", name, i)
		}
	}

	// 4. Artifact round trip preserves the model exactly.
	path := filepath.Join(t.TempDir(), name+".pgdac")
	if _, err := artifact.Build(path, to, artifact.BuildOptions{Name: name, Version: "1", Generator: "test"}); err != nil {
		t.Fatalf("%s: build artifact: %v", name, err)
	}
	opened, err := artifact.Open(path)
	if err != nil || !opened.IntegrityOK {
		t.Fatalf("%s: open artifact: err=%v integrity=%v", name, err, opened != nil && opened.IntegrityOK)
	}
	if rt, err := engine.Compare(opened.Model, to, opts); err != nil || !rt.Empty() {
		t.Errorf("%s: artifact round trip should be lossless (err=%v):\n%s", name, err, rt.Script(true))
	}
}
