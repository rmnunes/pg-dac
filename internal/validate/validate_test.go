package validate_test

import (
	"context"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/validate"
)

const projectSQL = `
CREATE SCHEMA app;
CREATE TYPE app.status AS ENUM ('active','inactive');
CREATE TABLE app.users (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email text NOT NULL,
  status app.status NOT NULL DEFAULT 'active',
  CONSTRAINT users_email_uniq UNIQUE (email)
);
CREATE TABLE app.orders (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES app.users (id),
  amount numeric(10,2) NOT NULL CHECK (amount >= 0)
);
CREATE INDEX orders_user_idx ON app.orders (user_id);
CREATE VIEW app.active_users AS SELECT id, email FROM app.users WHERE status = 'active';
`

// TestValidate_ProjectConverges exercises the whole of M2: it builds a model
// from authored SQL by materializing it in a scratch database (normalization),
// then validates that the migration from an empty schema to that project applies
// cleanly and converges. It needs the embedded PostgreSQL, so it is skipped in
// -short mode.
func TestValidate_ProjectConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scratch-database test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Share one scratch server across both builds and the validation, so this
	// exercises the shared-server path and only starts PostgreSQL once.
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	// "from" is an empty schema (the fresh-database baseline); "to" is the project.
	from, _, err := build.FromSQL(ctx, "", build.Options{})
	if err != nil {
		t.Fatalf("build empty baseline: %v", err)
	}
	to, _, err := build.FromSQL(ctx, projectSQL, build.Options{})
	if err != nil {
		t.Fatalf("build project model: %v", err)
	}

	// Sanity: the project model actually has the objects we authored.
	if got := schemaNames(to); !contains(got, "app") {
		t.Fatalf("expected an 'app' schema in the built model, got %v", got)
	}

	res, err := validate.Plan(ctx, from, to, engine.DefaultOptions())
	if err != nil {
		t.Fatalf("validation failed to apply: %v", err)
	}
	if !res.Converged {
		t.Fatalf("migration did not converge; %d residual statement(s):\n%s",
			len(res.Residual.Statements), res.Residual.Script(true))
	}
}

func schemaNames(d *model.Database) []string {
	var out []string
	for _, s := range d.Schemas {
		out = append(out, s.Name)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
