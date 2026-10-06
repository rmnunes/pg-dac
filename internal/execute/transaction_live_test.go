package execute_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// In single-transaction mode a failing statement rolls its whole batch back,
// statements that cannot run in a transaction block run on their own between
// batches, and a statement_timeout is honored.
func TestSingleTransactionBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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

	tx := func(sql string) sqlgen.Statement { return sqlgen.Statement{SQL: sql, Transactional: true} }
	opts := execute.DefaultOptions()
	opts.SingleTransaction = true

	// Batch 1 (t1, t2) commits; the CONCURRENTLY index runs alone; batch 2
	// (t3, then a failure) rolls back entirely: t3 must not exist.
	stmts := []sqlgen.Statement{
		tx("CREATE TABLE t1(a int);"),
		tx("CREATE TABLE t2(a int);"),
		{SQL: "CREATE INDEX CONCURRENTLY t1_a ON t1(a);", Transactional: false},
		tx("CREATE TABLE t3(a int);"),
		tx("CREATE TABLE t3(a int);"), // duplicate: fails
	}
	applied, err := execute.Apply(ctx, conn, stmts, opts)
	if err == nil {
		t.Fatal("expected the duplicate CREATE TABLE to fail")
	}
	if applied != 3 {
		t.Fatalf("want 3 committed statements (two tables + the concurrent index), got %d", applied)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename IN ('t1','t2','t3')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("t3 must have been rolled back with its batch; tables present: %d", n)
	}
	var idx int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 't1_a'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("the concurrent index must exist: %d %v", idx, err)
	}

	// statement_timeout applies.
	opts.StatementTimeout = 200 * time.Millisecond
	if _, err := execute.Apply(ctx, conn, []sqlgen.Statement{tx("SELECT pg_sleep(2);")}, opts); err == nil {
		t.Fatal("statement_timeout should have cancelled the statement")
	}
}
