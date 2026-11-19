package dbconn

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestConnect_RetriesTransientAndNamesAttempts: an unresolvable host is a
// transient failure (the resolver may answer differently a moment later), so
// every attempt is made and the final error says how many; a connection
// refused on a closed port likewise. Both come back wrapped so callers can
// still inspect the underlying network error.
func TestConnect_RetriesTransientAndNamesAttempts(t *testing.T) {
	saved := Backoff
	Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { Backoff = saved }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := Connect(ctx, "postgres://u@nonexistent-host.invalid:5432/db?connect_timeout=3")
	if err == nil {
		t.Fatal("expected a DNS failure")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Errorf("error should wrap *net.DNSError: %v", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error should name the attempt count: %v", err)
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
	saved := Backoff
	Backoff = []time.Duration{time.Millisecond}
	defer func() { Backoff = saved }()
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
	saved := Backoff
	Backoff = []time.Duration{time.Hour}
	defer func() { Backoff = saved }()
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
