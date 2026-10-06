package devdb

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestStartStop verifies an embedded scratch PostgreSQL can be started, accepts
// a connection, and stops cleanly. It is skipped in -short mode because the
// first run downloads the PostgreSQL binaries.
func TestStartStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	inst, err := Start(ctx, DefaultConfig())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer inst.Stop()

	conn, err := pgx.Connect(ctx, inst.ConnString())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1, got %d", n)
	}
}
