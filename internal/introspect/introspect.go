// Package introspect builds the canonical schema model from a live PostgreSQL
// database by querying pg_catalog (never information_schema, which omits
// PostgreSQL features and can return duplicate constraint rows). This is the
// authoritative model source: a declarative .sql project or a .pgdac artifact is
// reduced to the same model by materializing it in a scratch database and
// introspecting it through this same path, so both sides of a diff canonicalize
// identically.
//
// Objects are keyed on fully-qualified names; OIDs are used only transiently to
// join catalogs within a single introspection. Extension-owned (pg_depend
// deptype 'e') and internal (deptype 'i') objects are filtered out — emitting
// them would produce broken migrations.
package introspect

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rodrigonunes/pg-dac/internal/model"
)

// Querier is the subset of pgx used by the introspector. Both *pgx.Conn and
// *pgxpool.Pool satisfy it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options configures introspection.
type Options struct {
	// IncludeSchemas restricts introspection to the named schemas. When empty,
	// all user schemas (everything except pg_catalog, pg_toast, pg_temp_*, and
	// information_schema) are included.
	IncludeSchemas []string
}

// IntrospectWithWarnings is Introspect plus a list of human-readable warnings
// about object types that are present in the included schemas but that pg-dac
// does not yet model (and therefore silently ignores in a comparison). Surfacing
// these is what keeps a "no differences" result honest on a real schema.
func IntrospectWithWarnings(ctx context.Context, q Querier, opts Options) (*model.Database, []string, error) {
	db, err := Introspect(ctx, q, opts)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(db.Schemas))
	for _, s := range db.Schemas {
		names = append(names, s.Name)
	}
	warns, err := collectWarnings(ctx, q, names)
	if err != nil {
		return nil, nil, err
	}
	return db, warns, nil
}

// collectWarnings counts objects of types pg-dac does not manage within the
// given schemas, so callers can tell the user what is being ignored.
func collectWarnings(ctx context.Context, q Querier, schemas []string) ([]string, error) {
	checks := []struct {
		label string
		sql   string
	}{
		{"aggregate or window functions", `
SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname = ANY($1) AND p.prokind IN ('a','w')
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_catalog.pg_proc'::regclass AND d.objid=p.oid AND d.deptype IN ('e','i'))`},
		{"domain types", `
SELECT count(*) FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
WHERE n.nspname = ANY($1) AND t.typtype='d'
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_catalog.pg_type'::regclass AND d.objid=t.oid AND d.deptype IN ('e','i'))`},
		{"composite types", `
SELECT count(*) FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
JOIN pg_catalog.pg_class c ON c.oid=t.typrelid
WHERE n.nspname = ANY($1) AND t.typtype='c' AND c.relkind='c'
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_catalog.pg_type'::regclass AND d.objid=t.oid AND d.deptype IN ('e','i'))`},
		{"range or multirange types", `
SELECT count(*) FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
WHERE n.nspname = ANY($1) AND t.typtype IN ('r','m')
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_catalog.pg_type'::regclass AND d.objid=t.oid AND d.deptype IN ('e','i'))`},
		{"foreign tables", `
SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind='f'`},
		{"non-view rules", `
SELECT count(*) FROM pg_catalog.pg_rewrite r JOIN pg_catalog.pg_class c ON c.oid=r.ev_class
JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname = ANY($1) AND r.ev_type <> '1'`},
		{"partitioned tables (partition attachments are not managed)", `
SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind='p'`},
	}

	var warns []string
	for _, c := range checks {
		var n int
		if err := q.QueryRow(ctx, c.sql, schemas).Scan(&n); err != nil {
			return nil, fmt.Errorf("scan unsupported-object count (%s): %w", c.label, err)
		}
		if n > 0 {
			warns = append(warns, fmt.Sprintf("%d %s present but not managed by pg-dac", n, c.label))
		}
	}
	return warns, nil
}

// Introspect reads the schema of the connected database into a model.
func Introspect(ctx context.Context, q Querier, opts Options) (*model.Database, error) {
	db := &model.Database{}

	if err := q.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&db.TargetVersion); err != nil {
		return nil, fmt.Errorf("read server version: %w", err)
	}

	schemas, err := readSchemas(ctx, q, opts)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(schemas))
	byName := map[string]*model.Schema{}
	for _, s := range schemas {
		names = append(names, s.Name)
		byName[s.Name] = s
		db.Schemas = append(db.Schemas, s)
	}

	if db.Extensions, err = readExtensions(ctx, q, names); err != nil {
		return nil, err
	}
	if err := readEnums(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readCollations(ctx, q, names, db.MajorVersion(), byName); err != nil {
		return nil, err
	}
	if err := readSequences(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readFunctions(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readTables(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readViews(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readMatViews(ctx, q, names, byName); err != nil {
		return nil, err
	}

	db.Sort()
	return db, nil
}

func readFunctions(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname,
       p.proname,
       CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END,
       pg_catalog.pg_get_function_identity_arguments(p.oid),
       pg_catalog.pg_get_functiondef(p.oid),
       l.lanname,
       COALESCE(pg_catalog.obj_description(p.oid, 'pg_proc'), '')
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
JOIN pg_catalog.pg_language l ON l.oid = p.prolang
WHERE p.prokind IN ('f', 'p')      -- functions and procedures (not aggregates/window)
  AND n.nspname = ANY($1)
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_proc'::regclass
          AND d.objid = p.oid AND d.deptype IN ('e', 'i'))
ORDER BY n.nspname, p.proname`, schemas)
	if err != nil {
		return fmt.Errorf("read functions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		f := &model.Function{}
		if err := rows.Scan(&f.Schema, &f.Name, &f.Kind, &f.IdentityArgs, &f.Definition, &f.Language, &f.Comment); err != nil {
			return err
		}
		f.Definition = nlnorm(f.Definition)
		if s := byName[f.Schema]; s != nil {
			s.Functions = append(s.Functions, f)
		}
	}
	return rows.Err()
}

func readSchemas(ctx context.Context, q Querier, opts Options) ([]*model.Schema, error) {
	const base = `
SELECT n.nspname,
       pg_catalog.pg_get_userbyid(n.nspowner) AS owner,
       COALESCE(pg_catalog.obj_description(n.oid, 'pg_namespace'), '')
FROM pg_catalog.pg_namespace n
WHERE %s
ORDER BY n.nspname`

	var (
		rows pgx.Rows
		err  error
	)
	if len(opts.IncludeSchemas) > 0 {
		rows, err = q.Query(ctx, fmt.Sprintf(base, "n.nspname = ANY($1)"), opts.IncludeSchemas)
	} else {
		rows, err = q.Query(ctx, fmt.Sprintf(base,
			"n.nspname NOT LIKE 'pg\\_%' AND n.nspname <> 'information_schema'"))
	}
	if err != nil {
		return nil, fmt.Errorf("read schemas: %w", err)
	}
	defer rows.Close()

	var out []*model.Schema
	for rows.Next() {
		s := &model.Schema{}
		if err := rows.Scan(&s.Name, &s.Owner, &s.Comment); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func readExtensions(ctx context.Context, q Querier, schemas []string) ([]*model.Extension, error) {
	rows, err := q.Query(ctx, `
SELECT e.extname,
       n.nspname,
       e.extversion,
       COALESCE(pg_catalog.obj_description(e.oid, 'pg_extension'), '')
FROM pg_catalog.pg_extension e
JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
WHERE n.nspname = ANY($1)
ORDER BY e.extname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read extensions: %w", err)
	}
	defer rows.Close()

	var out []*model.Extension
	for rows.Next() {
		e := &model.Extension{}
		if err := rows.Scan(&e.Name, &e.Schema, &e.Version, &e.Comment); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func readEnums(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname,
       t.typname,
       pg_catalog.pg_get_userbyid(t.typowner),
       COALESCE(pg_catalog.obj_description(t.oid, 'pg_type'), ''),
       array_agg(e.enumlabel ORDER BY e.enumsortorder)
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
JOIN pg_catalog.pg_enum e ON e.enumtypid = t.oid
WHERE t.typtype = 'e'
  AND n.nspname = ANY($1)
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_type'::regclass
          AND d.objid = t.oid AND d.deptype IN ('e', 'i'))
GROUP BY n.nspname, t.typname, t.typowner, t.oid
ORDER BY n.nspname, t.typname`, schemas)
	if err != nil {
		return fmt.Errorf("read enums: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		e := &model.EnumType{}
		if err := rows.Scan(&e.Schema, &e.Name, &e.Owner, &e.Comment, &e.Values); err != nil {
			return err
		}
		if s := byName[e.Schema]; s != nil {
			s.Enums = append(s.Enums, e)
		}
	}
	return rows.Err()
}
