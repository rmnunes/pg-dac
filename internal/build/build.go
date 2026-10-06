// Package build turns authored SQL — a single .sql file or a directory of them
// (a declarative project) — into a canonical schema model. It does this by
// applying the SQL to a throwaway scratch database and introspecting the result,
// so authored DDL canonicalizes through the exact same path as a live database.
// This is what makes a project-vs-database (or, later, artifact-vs-database)
// comparison free of cosmetic false positives.
package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rodrigonunes/pg-dac/internal/devdb"
	"github.com/rodrigonunes/pg-dac/internal/execute"
	"github.com/rodrigonunes/pg-dac/internal/introspect"
	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/project"
)

// Options configures a build.
type Options struct {
	// Schemas restricts the introspected model to these schemas (empty = all
	// user schemas present after applying the SQL).
	Schemas []string
	// TargetMajor selects the scratch PostgreSQL major version so authored SQL is
	// normalized against the version it targets (0 = library default). For a
	// project this is taken from the manifest.
	TargetMajor int
}

// FromSQL materializes authored SQL in a scratch database and returns its model
// plus any unsupported-object warnings.
func FromSQL(ctx context.Context, sqlText string, opts Options) (*model.Database, []string, error) {
	sc, err := devdb.Acquire(ctx, opts.TargetMajor)
	if err != nil {
		return nil, nil, err
	}
	defer sc.Release()

	conn, err := pgx.Connect(ctx, sc.ConnString)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to scratch db: %w", err)
	}
	defer conn.Close(ctx)

	// Defer function-body validation (as pg_dump does) so a function that
	// references a table/function created later in the script still applies — the
	// scratch database only needs the objects to exist, not the bodies to resolve.
	script := "SET check_function_bodies = false;\n" + sqlText
	if err := execute.ApplyScript(ctx, conn, script); err != nil {
		hint := ""
		if looksLikePsqlScript(sqlText) {
			hint = "\nhint: the source contains psql client-side meta-commands (e.g. \\i, \\echo, \\ir) " +
				"that the server cannot run. Point at the directory holding only schema DDL (e.g. a " +
				"deploy/ subfolder), or add a pgdac.yaml whose include globs list only schema files " +
				"(pre/post-deploy scripts are applied separately and excluded from the schema model)."
		}
		return nil, nil, fmt.Errorf("apply authored SQL to scratch db: %w%s", err, hint)
	}
	return introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: opts.Schemas})
}

// FromDir reads every .sql file under dir (recursively, in sorted path order),
// concatenates them, and builds a model from the result. File order within the
// concatenation does not affect the final model — the scratch database resolves
// dependencies — but a stable order keeps any apply error messages reproducible.
func FromDir(ctx context.Context, dir string, opts Options) (*model.Database, []string, error) {
	files, err := collectFiles(dir, &opts)
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("no .sql files found under %s", dir)
	}
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", f, err)
		}
		b.WriteString("-- file: " + f + "\n")
		b.Write(data)
		b.WriteString("\n")
	}
	return FromSQL(ctx, b.String(), opts)
}

// collectFiles returns the project's schema-body .sql files. When the directory
// has a pgdac.yaml it uses the manifest's ordered include globs (and applies its
// schema scope when the caller did not request specific schemas); otherwise it
// falls back to a recursive, sorted glob of all *.sql.
// looksLikePsqlScript reports whether the text appears to contain psql
// client-side meta-commands (a line whose first non-space character is a
// backslash followed by a letter). Used only to enrich an apply error with a
// hint — never to modify the SQL.
func looksLikePsqlScript(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		t := strings.TrimLeft(line, " \t")
		if len(t) >= 2 && t[0] == '\\' && ((t[1] >= 'a' && t[1] <= 'z') || (t[1] >= 'A' && t[1] <= 'Z')) {
			return true
		}
	}
	return false
}

func collectFiles(dir string, opts *Options) ([]string, error) {
	if project.Exists(dir) {
		m, err := project.Load(dir)
		if err != nil {
			return nil, err
		}
		if len(opts.Schemas) == 0 {
			opts.Schemas = m.Schemas
		}
		if opts.TargetMajor == 0 {
			opts.TargetMajor = m.TargetMajor
		}
		return m.IncludeFiles(dir)
	}
	return collectSQLFiles(dir)
}

func collectSQLFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".sql") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}
