package engine_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/introspect"
)

// TestIntrospectSelfIdempotent is the strongest cheap correctness invariant for
// the introspection + diff + sqlgen pipeline: introspecting any real database
// into a model M and then comparing M against itself must produce zero changes.
// A spurious difference means introspection is non-deterministic or sqlgen and
// the differ disagree about canonical form.
//
// It runs only when PG_DAC_TEST_DSN points at a reachable database, so the
// default `go test ./...` does not require Docker.
func TestIntrospectSelfIdempotent(t *testing.T) {
	dsn := os.Getenv("PG_DAC_TEST_DSN")
	if dsn == "" {
		t.Skip("set PG_DAC_TEST_DSN to a PostgreSQL connection string to run this integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	db, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}

	res, err := engine.Compare(db, db, engine.DefaultOptions())
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !res.Empty() {
		t.Fatalf("self-comparison produced %d statement(s); introspection/sqlgen disagree:\n%s",
			len(res.Statements), res.Script(true))
	}
}
