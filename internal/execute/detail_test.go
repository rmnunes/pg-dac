package execute

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// failingExecer accepts the session setup (SET …) and fails every other
// statement with err.
type failingExecer struct{ err error }

func (f failingExecer) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "SET ") {
		return pgconn.CommandTag{}, nil
	}
	return pgconn.CommandTag{}, f.err
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
