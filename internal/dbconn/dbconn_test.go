package dbconn

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func shortBackoff(t *testing.T, waits ...time.Duration) {
	t.Helper()
	saved := backoff
	backoff = waits
	t.Cleanup(func() { backoff = saved })
}

// TestConnect_RetriesTransientAndNamesAttempts: an unresolvable host is a
// transient failure (the resolver may answer differently a moment later), so
// every attempt is made and the final error says how many and which server;
// a connection refused on a closed port likewise. Both come back wrapped so
// callers can still inspect the underlying network error.
func TestConnect_RetriesTransientAndNamesAttempts(t *testing.T) {
	shortBackoff(t, time.Millisecond, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := Connect(ctx, "postgres://u:secret@nonexistent-host.invalid:5432/db?connect_timeout=3")
	if err == nil {
		t.Fatal("expected a DNS failure")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Errorf("error should wrap *net.DNSError: %v", err)
	}
	for _, want := range []string{"after 3 attempts", "connect to nonexistent-host.invalid:5432/db"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the password leaked into the error: %v", err)
	}

	// A closed port on loopback: refused, hence transient, hence retried.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	_, err = Connect(ctx, "postgres://u@"+addr+"/db?connect_timeout=3")
	if err == nil || !Transient(err) || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("refused dial should be retried and reported: %v", err)
	}
}

// TestConnect_NonTransientFailsAtOnce: a malformed connection string is not a
// network condition; no retry, no attempt count.
func TestConnect_NonTransientFailsAtOnce(t *testing.T) {
	shortBackoff(t, time.Millisecond)
	_, err := Connect(context.Background(), "postgres://u@host:notaport/db")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if Transient(err) || strings.Contains(err.Error(), "attempts") {
		t.Errorf("a parse error must not be retried: %v", err)
	}
}

// TestConnect_StopsWhenContextEnds: cancellation between attempts returns the
// last error without waiting out the backoff.
func TestConnect_StopsWhenContextEnds(t *testing.T) {
	shortBackoff(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := Connect(ctx, "postgres://u@nonexistent-host.invalid:5432/db?connect_timeout=3")
	if err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("did not stop at cancellation")
	}
}

// TestTransient classifies by what the failure means, not by where it was
// seen: what heals by waiting is retried, what the server decided is not.
func TestTransient(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: code} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "x"}, true},
		{"dial refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"read reset", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		{"closed during startup", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"server starting up (57P03)", pg("57P03"), true},
		{"too many connections (53300)", pg("53300"), true},
		{"connection exception (08006)", pg("08006"), true},
		{"bad password (28P01)", pg("28P01"), false},
		{"unknown database (3D000)", pg("3D000"), false},
		{"wrapped auth failure", errors.Join(errors.New("ctx"), pg("28000")), false},
		{"plain error", errors.New("something"), false},
	}
	for _, c := range cases {
		if got := Transient(c.err); got != c.want {
			t.Errorf("%s: Transient = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"postgres://user:pw@db.example.internal:5433/app?sslmode=require": "db.example.internal:5433/app",
		"host=localhost dbname=x user=u password=p":                       "localhost:5432/x",
		"not a connection string at all ://":                              "database",
	}
	for in, want := range cases {
		if got := Describe(in); got != want {
			t.Errorf("Describe(%q) = %q, want %q", in, got, want)
		}
	}
}

// Sessions are UTF-8 whatever the environment asks for: pgx reads
// PGCLIENTENCODING, and a non-UTF-8 session would make the server misread
// the UTF-8 text pgx sends.
func TestParseConfigForcesUTF8(t *testing.T) {
	t.Setenv("PGCLIENTENCODING", "WIN1252")
	cfg, err := ParseConfig("postgres://app@localhost:5432/app")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RuntimeParams["client_encoding"]; got != "UTF8" {
		t.Fatalf("client_encoding = %q, want UTF8", got)
	}
}
