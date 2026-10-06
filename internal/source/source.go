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
	// TargetMajor selects the scratch PostgreSQL major used to normalize a
	// project/.sql source (0 = the project's manifest, else the library default).
	TargetMajor int
	// RequireKey, when set, is the signer fingerprint a .pgdac source must carry.
	RequireKey string
	// TrustedKeys, when non-empty (and RequireKey is unset), is the set of
	// signer fingerprints a .pgdac source may carry.
	TrustedKeys []string
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
		return resolveArtifact(spec, opts)
	}

	if fi, err := os.Stat(spec); err == nil {
		bopts := build.Options{Schemas: opts.Schemas, TargetMajor: opts.TargetMajor}
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

// resolveArtifact opens a .pgdac, rejecting it if its integrity check fails, if
// it is signed but the signature does not verify, or if its signer is not the
// one required / not in the trust list. Without any trust configuration a
// signed artifact is accepted and its signer fingerprint reported, and an
// unsigned one is accepted with a warning. An artifact written by an older
// format version is accepted with a warning: its canonical form may differ from
// a freshly introspected model, which shows up as spurious differences until it
// is rebuilt.
func resolveArtifact(path string, opts Options) (*model.Database, []string, error) {
	o, err := artifact.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if !o.IntegrityOK {
		return nil, nil, fmt.Errorf("artifact %s failed its integrity check (corrupt or tampered)", path)
	}
	if o.Signed && !o.SignatureOK {
		return nil, nil, fmt.Errorf("artifact %s has an invalid signature", path)
	}
	var warns []string
	fp := o.Fingerprint()
	switch {
	case opts.RequireKey != "":
		if !o.Signed {
			return nil, nil, fmt.Errorf("artifact %s is unsigned but key %s is required", path, opts.RequireKey)
		}
		if fp != opts.RequireKey {
			return nil, nil, fmt.Errorf("artifact %s is signed by key %s, not the required %s", path, fp, opts.RequireKey)
		}
	case len(opts.TrustedKeys) > 0:
		if !o.Signed {
			return nil, nil, fmt.Errorf("artifact %s is unsigned but a trusted-keys list is configured", path)
		}
		trusted := false
		for _, k := range opts.TrustedKeys {
			if k == fp {
				trusted = true
			}
		}
		if !trusted {
			return nil, nil, fmt.Errorf("artifact %s is signed by key %s, which is not in the trusted-keys list", path, fp)
		}
	case !o.Signed:
		warns = append(warns, fmt.Sprintf("artifact %s is unsigned; its origin cannot be verified", path))
	default:
		warns = append(warns, fmt.Sprintf("artifact %s is signed by key %s (no trust list configured — pass --require-key or add the key to the trusted_keys file to enforce it)", path, fp))
	}
	if o.Manifest.FormatVersion < artifact.FormatVersion {
		warns = append(warns, fmt.Sprintf("artifact %s uses format version %d (current %d); its canonical "+
			"definitions may differ in schema qualification — rebuild it to avoid spurious differences",
			path, o.Manifest.FormatVersion, artifact.FormatVersion))
	}
	return o.Model, warns, nil
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
