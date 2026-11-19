// Package dbconn opens connections to live databases with a short retry on
// transient failures: a name lookup that fails once (split-DNS resolvers such
// as Cloudflare WARP answer the first query for a private name with NXDOMAIN
// and only then cache the real answer), a dial refused or reset while a
// gateway restarts. That is the connection resiliency SqlClient gives
// SqlPackage and what Azure's guidance for PostgreSQL clients asks for.
// Authentication, TLS and protocol errors are returned at once: retrying a
// wrong password could lock an account, and they do not heal by waiting.
package dbconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

// Backoff holds the waits between attempts; its length plus one is the number
// of attempts. Package-level so tests can shorten it.
var Backoff = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// Connect opens a connection, retrying transient dial failures per Backoff.
// The returned error names the attempt count when more than one was made.
func Connect(ctx context.Context, connString string) (*pgx.Conn, error) {
	var last error
	for attempt := 0; ; attempt++ {
		conn, err := pgx.Connect(ctx, connString)
		if err == nil {
			return conn, nil
		}
		last = err
		if !Transient(err) || attempt >= len(Backoff) {
			if attempt > 0 {
				return nil, fmt.Errorf("%w (after %d attempts)", last, attempt+1)
			}
			return nil, last
		}
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(Backoff[attempt]):
		}
	}
}

// Transient reports whether a connect error is worth retrying: a DNS failure
// or a dial that was refused, reset or timed out before any PostgreSQL
// protocol exchange. Anything the server answered (bad password, unknown
// database, TLS rejection) is not.
func Transient(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}
