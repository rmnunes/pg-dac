package execute

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsLockTimeout(t *testing.T) {
	if !isLockTimeout(&pgconn.PgError{Code: "55P03"}) {
		t.Error("55P03 (lock_not_available) should be detected as a lock timeout")
	}
	if isLockTimeout(&pgconn.PgError{Code: "42P01"}) {
		t.Error("42P01 (undefined_table) is not a lock timeout")
	}
	if isLockTimeout(errors.New("some other error")) {
		t.Error("a non-pg error is not a lock timeout")
	}
}

func TestBackoffBounds(t *testing.T) {
	opts := Options{BaseBackoff: 100 * time.Millisecond, MaxBackoff: 5 * time.Second}
	for attempt := 0; attempt < 12; attempt++ {
		for i := 0; i < 200; i++ {
			d := backoff(opts, attempt)
			if d < 0 {
				t.Fatalf("attempt %d: negative backoff %v", attempt, d)
			}
			if d > opts.MaxBackoff {
				t.Fatalf("attempt %d: backoff %v exceeds cap %v", attempt, d, opts.MaxBackoff)
			}
		}
	}
}
