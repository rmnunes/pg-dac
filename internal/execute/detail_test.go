package execute

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// failingExecer accepts the session setup (SET …) and transaction control,
// and fails every other statement with err.
type failingExecer struct{ err error }

func (f failingExecer) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.HasPrefix(sql, "SET "), sql == "BEGIN", sql == "COMMIT", sql == "ROLLBACK":
		return pgconn.CommandTag{}, nil
	}
	return pgconn.CommandTag{}, f.err
}

func dependsErr() *pgconn.PgError {
	return &pgconn.PgError{
		Severity: "ERROR",
		Code:     "2BP01",
		Message:  "cannot drop view sales.v because other objects depend on it",
		Detail:   "view pg_temp_7.scratch depends on view sales.v",
		Hint:     "Use DROP ... CASCADE to drop the dependent objects too.",
	}
}

func wantDetail(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a failure", what)
	}
	for _, want := range []string{"DETAIL: view pg_temp_7.scratch depends on view sales.v", "HINT: Use DROP ... CASCADE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q lacks %q", what, err, want)
		}
	}
}

// TestEveryFailurePathCarriesServerDetail: the streaming apply, the batched
// (single-transaction) apply and the deferred project apply all report
// DETAIL and HINT the same way.
func TestEveryFailurePathCarriesServerDetail(t *testing.T) {
	ctx := context.Background()
	stmts := []sqlgen.Statement{{SQL: "DROP VIEW sales.v;", Transactional: true}}
	opts := DefaultOptions()
	opts.MaxRetries = 0

	_, err := ApplyStream(ctx, failingExecer{err: dependsErr()}, stmts, opts, nil)
	wantDetail(t, err, "stream")

	opts.SingleTransaction = true
	_, err = ApplyStream(ctx, failingExecer{err: dependsErr()}, stmts, opts, nil)
	wantDetail(t, err, "batched")
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("batched: %v", err)
	}

	err = ApplyDeferred(ctx, failingExecer{err: dependsErr()}, []string{"DROP VIEW sales.v;"})
	wantDetail(t, err, "deferred")
}

// TestStatementFailureCarriesServerDetail: PostgreSQL's DETAIL and HINT name
// what the message alone does not (which object blocks a drop); they must
// survive into the error the CLI prints and the GUI shows, and the PgError
// must still be reachable for callers that inspect the SQLSTATE.
func TestStatementFailureCarriesServerDetail(t *testing.T) {
	pgErr := &pgconn.PgError{
		Severity: "ERROR",
		Code:     "2BP01",
		Message:  "cannot drop view sales.v because other objects depend on it",
		Detail:   "view pg_temp_7.scratch depends on view sales.v",
		Hint:     "Use DROP ... CASCADE to drop the dependent objects too.",
	}
	err := ApplyStatements(context.Background(), failingExecer{err: pgErr}, []sqlgen.Statement{{SQL: "DROP VIEW sales.v;", Transactional: true}})
	if err == nil {
		t.Fatal("expected the statement to fail")
	}
	msg := err.Error()
	for _, want := range []string{"statement failed: DROP VIEW sales.v;", "DETAIL: view pg_temp_7.scratch depends on view sales.v", "HINT: Use DROP ... CASCADE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	var got *pgconn.PgError
	if !errors.As(err, &got) || got.Code != "2BP01" {
		t.Errorf("PgError must stay in the chain: %v", err)
	}

	// No detail, no suffix.
	plain := &pgconn.PgError{Severity: "ERROR", Code: "42P01", Message: "relation x does not exist"}
	err = ApplyStatements(context.Background(), failingExecer{err: plain}, []sqlgen.Statement{{SQL: "SELECT 1;", Transactional: true}})
	if err == nil || strings.Contains(err.Error(), "DETAIL") || strings.Contains(err.Error(), "HINT") {
		t.Errorf("unexpected suffix: %v", err)
	}
}
