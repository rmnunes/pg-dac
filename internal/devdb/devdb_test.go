package devdb

import (
	"context"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
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

	inst, err := Start(ctx, Config{})
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

// TestStartWhileAnotherRuns is the CLI-next-to-GUI case: a second scratch
// server of the same major must start while the first one is running — the
// shared binaries cache is complete and is not extracted over again.
func TestStartWhileAnotherRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	a, err := Start(ctx, Config{})
	if err != nil {
		t.Fatalf("start first: %v", err)
	}
	defer a.Stop()
	b, err := Start(ctx, Config{})
	if err != nil {
		t.Fatalf("start second while the first runs: %v", err)
	}
	defer b.Stop()
	for _, inst := range []*Instance{a, b} {
		conn, err := pgx.Connect(ctx, inst.ConnString())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		var n int
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
			t.Fatalf("query: %v", err)
		}
		conn.Close(ctx)
	}
}

// The scratch cluster is UTF-8 with the C locale on every platform, so text a
// UTF-8 session sends is stored as written; initdb would otherwise follow the
// OS locale (WIN1252 on Windows) and reject what that code page lacks.
func TestScratchIsUTF8(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres test in -short mode")
	}
	t.Setenv("PGCLIENTENCODING", "UTF8")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx, cleanup := WithShared(ctx)
	defer cleanup()

	sc, err := Acquire(ctx, 16)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer sc.Release()
	conn, err := pgx.Connect(ctx, sc.ConnString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var server, ctype string
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_encoding'), datctype
		FROM pg_database WHERE datname = current_database()`).Scan(&server, &ctype); err != nil {
		t.Fatalf("query: %v", err)
	}
	if server != "UTF8" || ctype != "C" {
		t.Fatalf("server_encoding=%s lc_ctype=%s, want UTF8 and C", server, ctype)
	}
	var text string
	if err := conn.QueryRow(ctx, `SELECT 'em — dash → ✓'`).Scan(&text); err != nil || text != "em — dash → ✓" {
		t.Fatalf("round trip = %q, %v", text, err)
	}
}

// PGDAC_SCRATCH_MAJOR picks the scratch major when nothing names one (CI runs
// the suite once per supported major); an explicit major always wins, and a
// value outside the supported range is ignored.
func TestDefaultMajor(t *testing.T) {
	cases := map[string]embeddedpostgres.PostgresVersion{
		"":    embeddedpostgres.V16,
		"13":  embeddedpostgres.V13,
		"11":  embeddedpostgres.V11,
		"18":  embeddedpostgres.V18,
		"10":  embeddedpostgres.V16,
		"99":  embeddedpostgres.V16,
		"abc": embeddedpostgres.V16,
	}
	for env, want := range cases {
		t.Setenv("PGDAC_SCRATCH_MAJOR", env)
		if got := VersionFor(0); got != want {
			t.Errorf("PGDAC_SCRATCH_MAJOR=%q: VersionFor(0) = %s, want %s", env, got, want)
		}
		if got := VersionFor(17); got != embeddedpostgres.V17 {
			t.Errorf("PGDAC_SCRATCH_MAJOR=%q: an explicit 17 became %s", env, got)
		}
	}
}
