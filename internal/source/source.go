// Package source resolves a user-supplied source spec into a schema model. A
// spec is one of: a PostgreSQL connection string (a live database), a .pgdac
// artifact path (added in a later milestone), or a declarative project
// directory (added in a later milestone). Resolving every spec to a model is
// what lets the CLI verbs treat DB-to-DB, artifact-to-DB, and artifact-to-
// artifact comparisons uniformly.
package source

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rodrigonunes/pg-dac/internal/artifact"
	"github.com/rodrigonunes/pg-dac/internal/build"
	"github.com/rodrigonunes/pg-dac/internal/introspect"
	"github.com/rodrigonunes/pg-dac/internal/model"
)

// Options configures resolution.
type Options struct {
	// Schemas restricts a live-database source to the named schemas (empty = all
	// user schemas).
	Schemas []string
}

// Resolve turns a source spec into a model and any unsupported-object warnings.
// A spec is a PostgreSQL connection string, a path to a declarative project
// directory, a path to a single .sql file, or a .pgdac artifact.
func Resolve(ctx context.Context, spec string, opts Options) (*model.Database, []string, error) {
	spec = strings.TrimPrefix(spec, "file://")
	switch {
	case IsConnString(spec):
		return resolveDB(ctx, spec, opts)
	case strings.HasSuffix(spec, ".pgdac"):
		m, err := resolveArtifact(spec)
		return m, nil, err
	}

	if fi, err := os.Stat(spec); err == nil {
		bopts := build.Options{Schemas: opts.Schemas}
		if fi.IsDir() {
			return build.FromDir(ctx, spec, bopts)
		}
		if strings.EqualFold(extOf(spec), ".sql") {
			data, err := os.ReadFile(spec)
			if err != nil {
				return nil, nil, fmt.Errorf("read %s: %w", spec, err)
			}
			return build.FromSQL(ctx, string(data), bopts)
		}
		return nil, nil, fmt.Errorf("unsupported source file %q: expected a directory or a .sql file", spec)
	}

	return nil, nil, fmt.Errorf("unrecognized source %q: expected a PostgreSQL connection string "+
		"(postgres://... or \"host=... dbname=...\"), a project directory, or a .sql file", spec)
}

func extOf(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i:]
	}
	return ""
}

// IsConnString reports whether spec looks like a PostgreSQL connection string
// (as opposed to a project directory, a .sql file, or an artifact).
func IsConnString(s string) bool {
	return strings.HasPrefix(s, "postgres://") ||
		strings.HasPrefix(s, "postgresql://") ||
		strings.Contains(s, "host=") ||
		strings.Contains(s, "dbname=") ||
		strings.Contains(s, "user=")
}

// resolveArtifact opens a .pgdac, rejecting it if its integrity check fails or
// if it is signed but the signature does not verify.
func resolveArtifact(path string) (*model.Database, error) {
	o, err := artifact.Open(path)
	if err != nil {
		return nil, err
	}
	if !o.IntegrityOK {
		return nil, fmt.Errorf("artifact %s failed its integrity check (corrupt or tampered)", path)
	}
	if o.Signed && !o.SignatureOK {
		return nil, fmt.Errorf("artifact %s has an invalid signature", path)
	}
	return o.Model, nil
}

func resolveDB(ctx context.Context, spec string, opts Options) (*model.Database, []string, error) {
	conn, err := pgx.Connect(ctx, spec)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	defer conn.Close(ctx)

	db, warns, err := introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: opts.Schemas})
	if err != nil {
		return nil, nil, fmt.Errorf("introspect database: %w", err)
	}
	return db, warns, nil
}
