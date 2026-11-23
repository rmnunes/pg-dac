// Package dbconn opens connections to live databases with a short retry on
// transient failures: a name lookup that fails once (split-DNS resolvers such
// as Cloudflare WARP answer the first query for a private name with NXDOMAIN
// and only then cache the real answer), a dial refused or reset while a
// gateway restarts, a connection the server closed during startup, or a
// server that is still starting up or momentarily full. That is the
// connection resiliency SqlClient gives SqlPackage and what Azure's guidance
// for PostgreSQL clients asks for. Authentication, TLS and protocol errors are
// returned at once: retrying a wrong password could lock an account, and they
// do not heal by waiting.
package dbconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// backoff holds the waits between attempts; its length plus one is the number
// of attempts. Tests shorten it.
var backoff = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// Connect opens a connection, retrying transient failures (see Transient).
// The error names the server (never the password) and the attempt count when
// more than one was made.
func Connect(ctx context.Context, connString string) (*pgx.Conn, error) {
	cfg, err := ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", Describe(connString), err)
	}
	var last error
	for attempt := 0; ; attempt++ {
		conn, err := pgx.ConnectConfig(ctx, cfg.Copy())
		if err == nil {
			return conn, nil
		}
		last = err
		if !Transient(err) || attempt >= len(backoff) {
			if attempt > 0 {
				last = fmt.Errorf("%w (after %d attempts)", last, attempt+1)
			}
			return nil, fmt.Errorf("connect to %s: %w", Describe(connString), last)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to %s: %w", Describe(connString), last)
		case <-time.After(backoff[attempt]):
		}
	}
}

// ParseConfig parses a connection string into the configuration every pg-dac
// connection uses. The session always runs with client_encoding UTF8: pgx
// sends and expects UTF-8 text, and without the setting the session takes the
// client encoding from PGCLIENTENCODING or, failing that, the database's own
// encoding (WIN1252 for a database created on Windows), so the server would
// misread or reject every non-ASCII character in a definition, a comment or a
// deploy script.
func ParseConfig(connString string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(connString)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["client_encoding"] = "UTF8"
	return cfg, nil
}

// Transient reports whether a connect error is worth retrying: a DNS failure;
// a dial, read or write that failed before the handshake completed (refused,
// reset, timed out, closed by the server); or a server that answered that it
// cannot accept connections right now — SQLSTATE 57P03 (starting up or
// shutting down), 53300 (too many connections) or class 08 (connection
// exception). Anything else the server answered (bad password, unknown
// database, TLS rejection) is not.
func Transient(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "57P03" || pgErr.Code == "53300" || strings.HasPrefix(pgErr.Code, "08")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// LooksLikeConnString reports whether a source spec is a connection string
// rather than a path (the same test source.IsConnString applies).
func LooksLikeConnString(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") ||
		strings.Contains(s, "host=") || strings.Contains(s, "dbname=") || strings.Contains(s, "user=")
}

// Redact is how a source spec may be shown, logged or stored: a connection
// string becomes the server it names (Describe), which carries no password;
// a path or an artifact name is returned as it is.
func Redact(spec string) string {
	if LooksLikeConnString(spec) {
		return Describe(spec)
	}
	return spec
}

// Describe names the server a connection string points at — host, port and
// database — without its password, for error messages.
func Describe(connString string) string {
	cfg, err := pgx.ParseConfig(connString)
	if err != nil {
		return "database"
	}
	host := cfg.Host
	if cfg.Port != 0 {
		host = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	}
	if cfg.Database != "" {
		return host + "/" + cfg.Database
	}
	return host
}
