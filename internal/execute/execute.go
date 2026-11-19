// Package execute applies generated statements to a database. This M2 version is
// deliberately minimal — it runs statements sequentially in autocommit, which is
// exactly what plan validation and scratch-database materialization need
// (per-statement autocommit also lets CONCURRENTLY statements run, since they
// cannot be inside a transaction block). Milestone 3 adds lock_timeout, retry
// with backoff, and transaction batching for production publishes.
package execute

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// Execer is the subset of pgx used to run individual statements.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Options tunes the lock-safety behavior of a production apply.
type Options struct {
	// LockTimeout is set as the session lock_timeout so blocking DDL aborts
	// quickly instead of queuing ahead of all later queries. 0 disables it.
	LockTimeout time.Duration
	// MaxRetries is how many times a statement that fails to acquire its lock
	// (SQLSTATE 55P03) is retried before giving up.
	MaxRetries int
	// BaseBackoff and MaxBackoff bound the randomized exponential backoff between
	// retries (backoff = random in [0, min(MaxBackoff, BaseBackoff*2^attempt)]).
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// StatementTimeout is set as the session statement_timeout. 0 leaves it
	// unbounded (long index builds and constraint validations must not be killed).
	StatementTimeout time.Duration
	// SingleTransaction runs consecutive transactional statements in one
	// transaction (SqlPackage's IncludeTransactionalScripts): a failure rolls the
	// open batch back, and a lock timeout inside it is retried as a whole. A
	// statement that cannot run inside a transaction block (CREATE INDEX
	// CONCURRENTLY, ...) commits what precedes it, runs on its own, and a new
	// batch starts after it — so with lock-safe rewrites disabled the whole
	// migration is one transaction, and with them enabled it is as atomic as
	// PostgreSQL allows.
	SingleTransaction bool
}

// DefaultOptions returns sensible production defaults: a short lock_timeout with
// a handful of jittered retries. statement_timeout is intentionally left
// unbounded so long index builds and constraint validations are not killed.
func DefaultOptions() Options {
	return Options{
		LockTimeout: 3 * time.Second,
		MaxRetries:  5,
		BaseBackoff: 100 * time.Millisecond,
		MaxBackoff:  5 * time.Second,
	}
}

// Progress is an optional callback invoked once per statement just before it is
// run (1-based index, total count, the statement). Used to stream publish
// progress to a GUI; pass nil to disable.
type Progress func(index, total int, s sqlgen.Statement)

// Apply runs statements against conn with lock-safety. Each statement runs in its
// own autocommit command, so CONCURRENTLY statements are never inside a
// transaction and a statement that fails to take its lock can be safely retried
// (it never committed). It returns the number of statements successfully applied;
// on failure the migration is left partially applied and can be resumed by
// recomputing the diff from the new current state and publishing again.
func Apply(ctx context.Context, conn Execer, stmts []sqlgen.Statement, opts Options) (int, error) {
	return ApplyStream(ctx, conn, stmts, opts, nil)
}

// ApplyStream is Apply with a per-statement progress callback (for streaming UIs).
func ApplyStream(ctx context.Context, conn Execer, stmts []sqlgen.Statement, opts Options, onStmt Progress) (int, error) {
	if err := prepareSession(ctx, conn); err != nil {
		return 0, err
	}
	if opts.LockTimeout > 0 {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = %d", opts.LockTimeout.Milliseconds())); err != nil {
			return 0, fmt.Errorf("set lock_timeout: %w", err)
		}
	}
	if opts.StatementTimeout > 0 {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", opts.StatementTimeout.Milliseconds())); err != nil {
			return 0, fmt.Errorf("set statement_timeout: %w", err)
		}
	}
	if opts.SingleTransaction {
		return applyBatched(ctx, conn, stmts, opts, onStmt)
	}
	applied := 0
	for i, s := range stmts {
		if onStmt != nil {
			onStmt(i+1, len(stmts), s)
		}
		if err := runWithRetry(ctx, conn, s.SQL, opts); err != nil {
			return applied, fmt.Errorf("statement failed: %s: %w%s", firstLine(s.SQL), err, serverDetail(err))
		}
		applied++
	}
	return applied, nil
}

// serverDetail appends PostgreSQL's DETAIL and HINT to a statement failure.
// They carry what the message alone does not — which object blocks a drop,
// which value violates a constraint — and are otherwise lost.
func serverDetail(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	out := ""
	if pgErr.Detail != "" {
		out += "; DETAIL: " + pgErr.Detail
	}
	if pgErr.Hint != "" {
		out += "; HINT: " + pgErr.Hint
	}
	return out
}

// applyBatched is ApplyStream in single-transaction mode (see
// Options.SingleTransaction). Returns the number of statements committed.
func applyBatched(ctx context.Context, conn Execer, stmts []sqlgen.Statement, opts Options, onStmt Progress) (int, error) {
	applied := 0
	i := 0
	for i < len(stmts) {
		if !stmts[i].Transactional {
			if onStmt != nil {
				onStmt(i+1, len(stmts), stmts[i])
			}
			if err := runWithRetry(ctx, conn, stmts[i].SQL, opts); err != nil {
				return applied, fmt.Errorf("statement failed: %s: %w%s", firstLine(stmts[i].SQL), err, serverDetail(err))
			}
			applied++
			i++
			continue
		}
		j := i
		for j < len(stmts) && stmts[j].Transactional {
			j++
		}
		if err := runBatch(ctx, conn, stmts, i, j, opts, onStmt); err != nil {
			return applied, err
		}
		applied += j - i
		i = j
	}
	return applied, nil
}

// runBatch runs stmts[from:to] inside one transaction, retrying the whole
// batch after a rollback when a lock timeout aborts it.
func runBatch(ctx context.Context, conn Execer, stmts []sqlgen.Statement, from, to int, opts Options, onStmt Progress) error {
	for attempt := 0; ; attempt++ {
		if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		var failed error
		var failedAt int
		for k := from; k < to; k++ {
			if onStmt != nil {
				onStmt(k+1, len(stmts), stmts[k])
			}
			if _, err := conn.Exec(ctx, stmts[k].SQL); err != nil {
				failed, failedAt = err, k
				break
			}
		}
		if failed == nil {
			if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
				return fmt.Errorf("commit transaction: %w", err)
			}
			return nil
		}
		_, _ = conn.Exec(ctx, "ROLLBACK")
		if !isLockTimeout(failed) || attempt >= opts.MaxRetries {
			return fmt.Errorf("statement failed (transaction rolled back, nothing from this batch applied): %s: %w%s", firstLine(stmts[failedAt].SQL), failed, serverDetail(failed))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(opts, attempt)):
		}
	}
}

func runWithRetry(ctx context.Context, conn Execer, sql string, opts Options) error {
	for attempt := 0; ; attempt++ {
		_, err := conn.Exec(ctx, sql)
		if err == nil {
			return nil
		}
		if !isLockTimeout(err) || attempt >= opts.MaxRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(opts, attempt)):
		}
	}
}

// isLockTimeout reports whether err is PostgreSQL's lock_not_available (55P03),
// the error raised when lock_timeout expires waiting for a lock.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return false
}

func backoff(opts Options, attempt int) time.Duration {
	max := opts.BaseBackoff << attempt
	if max <= 0 || max > opts.MaxBackoff {
		max = opts.MaxBackoff
	}
	return time.Duration(rand.Int63n(int64(max) + 1))
}

// prepareSession applies the session settings every pg-dac migration relies on
// (mirrored by the preamble at the top of generated scripts). Function bodies
// are not validated at CREATE time — as in pg_dump output — so a SQL-language
// function created in the "before tables" band may reference a table created
// later in the same migration.
func prepareSession(ctx context.Context, conn Execer) error {
	if _, err := conn.Exec(ctx, "SET check_function_bodies = false"); err != nil {
		return fmt.Errorf("set check_function_bodies: %w", err)
	}
	return nil
}

// ApplyStatements runs each statement in its own autocommit command. It stops at
// the first error and reports which statement failed.
func ApplyStatements(ctx context.Context, e Execer, stmts []sqlgen.Statement) error {
	if err := prepareSession(ctx, e); err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := e.Exec(ctx, s.SQL); err != nil {
			return fmt.Errorf("statement failed: %s: %w%s", firstLine(s.SQL), err, serverDetail(err))
		}
	}
	return nil
}

// ApplyDeferred runs authored statements one by one in autocommit, deferring
// any that fail and retrying them after the rest until a pass makes no
// progress. The order of a project's files therefore does not matter: a view
// that precedes its table fails once and succeeds on the next pass, exactly as
// a declarative SQL project behaves in SSDT. Statements that never succeed are
// reported together with their last error (up to a handful, then a count).
func ApplyDeferred(ctx context.Context, e Execer, stmts []string) error {
	if err := prepareSession(ctx, e); err != nil {
		return err
	}
	type pending struct {
		sql string
		err error
	}
	queue := make([]pending, 0, len(stmts))
	for _, s := range stmts {
		if strings.TrimSpace(s) != "" {
			queue = append(queue, pending{sql: s})
		}
	}
	for len(queue) > 0 {
		var failed []pending
		for _, p := range queue {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := e.Exec(ctx, p.sql); err != nil {
				failed = append(failed, pending{sql: p.sql, err: err})
			}
		}
		if len(failed) == 0 {
			return nil
		}
		if len(failed) == len(queue) {
			var b strings.Builder
			fmt.Fprintf(&b, "%d statement(s) failed after retrying in every order", len(failed))
			for i, p := range failed {
				if i == 5 {
					fmt.Fprintf(&b, "\n  ... and %d more", len(failed)-i)
					break
				}
				fmt.Fprintf(&b, "\n  %s: %v", firstLine(p.sql), p.err)
			}
			return errors.New(b.String())
		}
		queue = failed
	}
	return nil
}

// ApplyScript runs a multi-statement SQL script using the simple query protocol
// (one implicit transaction). Suitable for applying authored declarative DDL to
// a scratch database for normalization; not for migrations containing
// CONCURRENTLY statements.
func ApplyScript(ctx context.Context, conn *pgx.Conn, sql string) error {
	if strings.TrimSpace(sql) == "" {
		return nil
	}
	if err := conn.PgConn().Exec(ctx, sql).Close(); err != nil {
		return fmt.Errorf("apply script: %w", err)
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}
