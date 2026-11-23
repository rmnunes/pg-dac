package main

import (
	"os"
	"testing"
)

// A PGCLIENTENCODING meant for psql must not reach pg-dac's sessions or the
// embedded PostgreSQL library (see ignoreClientEncoding).
func TestIgnoreClientEncoding(t *testing.T) {
	t.Setenv("PGCLIENTENCODING", "WIN1252")
	ignoreClientEncoding()
	if v, ok := os.LookupEnv("PGCLIENTENCODING"); ok {
		t.Fatalf("PGCLIENTENCODING still set to %q", v)
	}
}
