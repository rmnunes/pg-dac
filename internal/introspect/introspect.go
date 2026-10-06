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
	"strings"

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

// collectWarnings counts objects of types — and properties — pg-dac does not
// manage within the given schemas, so callers can tell the user what is being
// ignored (and, for privileges, what a publish will not carry over).
func collectWarnings(ctx context.Context, q Querier, schemas []string) ([]string, error) {
	checks := []struct {
		label string
		sql   string
	}{
		{"aggregate or window functions", `
SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname = ANY($1) AND p.prokind IN ('a','w')
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid='pg_catalog.pg_proc'::regclass AND d.objid=p.oid AND d.deptype IN ('e','i'))`},
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
		{"tables using INHERITS (non-partition inheritance)", `
SELECT count(DISTINCT ih.inhrelid) FROM pg_catalog.pg_inherits ih
JOIN pg_catalog.pg_class c ON c.oid = ih.inhrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind IN ('r','p') AND NOT c.relispartition`},
		{"objects with explicit privileges (GRANT/REVOKE)", `
SELECT (SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
          WHERE n.nspname = ANY($1) AND c.relkind IN ('r','p','v','m','S') AND c.relacl IS NOT NULL)
     + (SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
          WHERE n.nspname = ANY($1) AND p.proacl IS NOT NULL)
     + (SELECT count(*) FROM pg_catalog.pg_namespace n
          WHERE n.nspname = ANY($1) AND n.nspname <> 'public' AND n.nspacl IS NOT NULL)
     + (SELECT count(*) FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
          WHERE n.nspname = ANY($1) AND t.typacl IS NOT NULL)`},
	}

	var warns []string
	for _, c := range checks {
		var n int
		if err := q.QueryRow(ctx, c.sql, schemas).Scan(&n); err != nil {
			return nil, fmt.Errorf("scan unsupported-object count (%s): %w", c.label, err)
		}
		if n > 0 {
			msg := fmt.Sprintf("%d %s present but not managed by pg-dac", n, c.label)
			if strings.HasPrefix(c.label, "objects with explicit privileges") {
				msg += " — privileges are not published"
			}
			warns = append(warns, msg)
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

	restore, err := pinSearchPath(ctx, q)
	if err != nil {
		return nil, err
	}
	defer restore()

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
	major := db.MajorVersion()

	if db.Extensions, err = readExtensions(ctx, q, names); err != nil {
		return nil, err
	}
	if err := readEnums(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readDomains(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readCompositeTypes(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readCollations(ctx, q, names, major, byName); err != nil {
		return nil, err
	}
	if err := readSequences(ctx, q, names, byName); err != nil {
		return nil, err
	}
	if err := readFunctions(ctx, q, names, byName); err != nil {
		return nil, err
	}
	tables, err := readTables(ctx, q, names, major, byName)
	if err != nil {
		return nil, err
	}
	views, err := readViews(ctx, q, names, byName)
	if err != nil {
		return nil, err
	}
	mviews, err := readMatViews(ctx, q, names, byName)
	if err != nil {
		return nil, err
	}
	if err := readIndexes(ctx, q, names, tables, mviews); err != nil {
		return nil, err
	}
	if err := readTriggers(ctx, q, names, major, tables, views); err != nil {
		return nil, err
	}

	db.Sort()
	return db, nil
}

// pinSearchPath sets an empty search_path for the duration of introspection
// (the same trick pg_dump uses) so that every pg_get_*def(), pg_get_expr() and
// format_type() output is fully schema-qualified. Without it, canonical
// definitions depend on the session's search_path: the same database read by
// two roles with different search_paths would produce two different models,
// and generated SQL would be unqualified and fragile. The previous value is
// restored on return so a connection reused for an apply is left untouched.
func pinSearchPath(ctx context.Context, q Querier) (func(), error) {
	var prev string
	if err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('search_path')`).Scan(&prev); err != nil {
		return nil, fmt.Errorf("read search_path: %w", err)
	}
	var ignored string
	if err := q.QueryRow(ctx, `SELECT pg_catalog.set_config('search_path', '', false)`).Scan(&ignored); err != nil {
		return nil, fmt.Errorf("pin search_path: %w", err)
	}
	return func() {
		var s string
		_ = q.QueryRow(context.Background(), `SELECT pg_catalog.set_config('search_path', $1, false)`, prev).Scan(&s)
	}, nil
}

func readFunctions(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) error {
	deps, err := readFunctionDeps(ctx, q, schemas)
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
SELECT n.nspname,
       p.proname,
       CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END,
       pg_catalog.pg_get_function_identity_arguments(p.oid),
       pg_catalog.pg_get_functiondef(p.oid),
       l.lanname,
       COALESCE(pg_catalog.pg_get_function_result(p.oid), ''),
       pg_catalog.pg_get_function_arguments(p.oid),
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
		if err := rows.Scan(&f.Schema, &f.Name, &f.Kind, &f.IdentityArgs, &f.Definition, &f.Language, &f.ReturnType, &f.Arguments, &f.Comment); err != nil {
			return err
		}
		f.Definition = nlnorm(f.Definition)
		f.DependsOn = deps[tkey(f.Schema, f.Signature())]
		if s := byName[f.Schema]; s != nil {
			s.Functions = append(s.Functions, f)
		}
	}
	return rows.Err()
}

// readFunctionDeps returns, for each function keyed by tkey(schema, signature),
// the objects pg_depend says it needs: relation row types and enum types in its
// signature, and relations / other functions referenced by a SQL-standard
// (BEGIN ATOMIC) body. These become planner edges so a function is created after
// the table or view whose row type it returns — the one ordering a fixed
// "functions before tables" band cannot express.
func readFunctionDeps(ctx context.Context, q Querier, schemas []string) (map[string][]model.ObjRef, error) {
	rows, err := q.Query(ctx, `
WITH f AS (
    SELECT p.oid, n.nspname,
           p.proname || '(' || pg_catalog.pg_get_function_identity_arguments(p.oid) || ')' AS sig
    FROM pg_catalog.pg_proc p
    JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
    WHERE p.prokind IN ('f', 'p') AND n.nspname = ANY($1)
)
-- relation row types (tables, views, matviews) used as argument/return types
SELECT f.nspname, f.sig, rn.nspname, rc.relname, rc.relkind::text
FROM pg_catalog.pg_depend d
JOIN f ON d.classid = 'pg_catalog.pg_proc'::regclass AND d.objid = f.oid
JOIN pg_catalog.pg_type t ON d.refclassid = 'pg_catalog.pg_type'::regclass AND d.refobjid = t.oid
JOIN pg_catalog.pg_class rc ON rc.oid = t.typrelid
JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
WHERE rc.relkind IN ('r', 'p', 'v', 'm')
UNION
-- enum types in the signature
SELECT f.nspname, f.sig, tn.nspname, t.typname, 'e'
FROM pg_catalog.pg_depend d
JOIN f ON d.classid = 'pg_catalog.pg_proc'::regclass AND d.objid = f.oid
JOIN pg_catalog.pg_type t ON d.refclassid = 'pg_catalog.pg_type'::regclass AND d.refobjid = t.oid
JOIN pg_catalog.pg_namespace tn ON tn.oid = t.typnamespace
WHERE t.typtype = 'e'
UNION
-- relations referenced directly (SQL-standard bodies)
SELECT f.nspname, f.sig, rn.nspname, rc.relname, rc.relkind::text
FROM pg_catalog.pg_depend d
JOIN f ON d.classid = 'pg_catalog.pg_proc'::regclass AND d.objid = f.oid
JOIN pg_catalog.pg_class rc ON d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjid = rc.oid
JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
WHERE rc.relkind IN ('r', 'p', 'v', 'm', 'S')
UNION
-- other functions referenced directly (SQL-standard bodies)
SELECT f.nspname, f.sig, fn.nspname,
       fp.proname || '(' || pg_catalog.pg_get_function_identity_arguments(fp.oid) || ')', 'f'
FROM pg_catalog.pg_depend d
JOIN f ON d.classid = 'pg_catalog.pg_proc'::regclass AND d.objid = f.oid
JOIN pg_catalog.pg_proc fp ON d.refclassid = 'pg_catalog.pg_proc'::regclass AND d.refobjid = fp.oid
JOIN pg_catalog.pg_namespace fn ON fn.oid = fp.pronamespace
WHERE fp.oid <> f.oid
ORDER BY 1, 2, 3, 4`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read function deps: %w", err)
	}
	defer rows.Close()

	out := map[string][]model.ObjRef{}
	for rows.Next() {
		var fschema, fsig, rschema, rname, rkind string
		if err := rows.Scan(&fschema, &fsig, &rschema, &rname, &rkind); err != nil {
			return nil, err
		}
		k := tkey(fschema, fsig)
		out[k] = append(out[k], model.ObjRef{Kind: relkindToObjKind(rkind), Schema: rschema, Name: rname})
	}
	return out, rows.Err()
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
		// pgdac holds pg-dac's own bookkeeping (refactorlog, baselines) and is
		// never part of a schema model.
		rows, err = q.Query(ctx, fmt.Sprintf(base,
			"n.nspname NOT LIKE 'pg\\_%' AND n.nspname <> 'information_schema' AND n.nspname <> 'pgdac'"))
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

// readDomains introspects domain types. PostgreSQL has no pg_get_domaindef, so
// the CREATE DOMAIN statement is reconstructed from the catalog (base type,
// collation, default, NOT NULL, named CHECK constraints in name order).
func readDomains(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname, t.typname,
       pg_catalog.pg_get_userbyid(t.typowner),
       pg_catalog.format_type(t.typbasetype, t.typtypmod),
       t.typnotnull,
       COALESCE(pg_catalog.pg_get_expr(t.typdefaultbin, 0), ''),
       CASE WHEN t.typcollation <> 0 AND t.typcollation <> bt.typcollation
            THEN (SELECT pg_catalog.quote_ident(cn.nspname) || '.' || pg_catalog.quote_ident(col.collname)
                  FROM pg_catalog.pg_collation col
                  JOIN pg_catalog.pg_namespace cn ON cn.oid = col.collnamespace
                  WHERE col.oid = t.typcollation)
            ELSE '' END,
       COALESCE((SELECT string_agg('CONSTRAINT ' || pg_catalog.quote_ident(con.conname) || ' ' || pg_catalog.pg_get_constraintdef(con.oid), ' ' ORDER BY con.conname)
                 FROM pg_catalog.pg_constraint con WHERE con.contypid = t.oid), ''),
       COALESCE(pg_catalog.obj_description(t.oid, 'pg_type'), '')
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
JOIN pg_catalog.pg_type bt ON bt.oid = t.typbasetype
WHERE t.typtype = 'd' AND n.nspname = ANY($1)
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_type'::regclass
          AND d.objid = t.oid AND d.deptype IN ('e', 'i'))
ORDER BY n.nspname, t.typname`, schemas)
	if err != nil {
		return fmt.Errorf("read domains: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ns, name, owner, base, def, coll, cons, comment string
		var notNull bool
		if err := rows.Scan(&ns, &name, &owner, &base, &notNull, &def, &coll, &cons, &comment); err != nil {
			return err
		}
		stmt := "CREATE DOMAIN " + model.QualifiedName(ns, name) + " AS " + base
		if coll != "" {
			stmt += " COLLATE " + coll
		}
		if def != "" {
			stmt += " DEFAULT " + def
		}
		if notNull {
			stmt += " NOT NULL"
		}
		if cons != "" {
			stmt += " " + nlnorm(cons)
		}
		d := &model.Domain{Schema: ns, Name: name, Owner: owner, Definition: stmt, Comment: comment}
		if s := byName[ns]; s != nil {
			s.Domains = append(s.Domains, d)
		}
	}
	return rows.Err()
}

// readCompositeTypes introspects stand-alone composite types (CREATE TYPE ...
// AS (...)); a table's implicit row type is relkind 'r' and excluded.
func readCompositeTypes(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname, t.typname,
       pg_catalog.pg_get_userbyid(t.typowner),
       COALESCE((SELECT string_agg(
                   pg_catalog.quote_ident(a.attname) || ' ' || pg_catalog.format_type(a.atttypid, a.atttypmod)
                   || CASE WHEN a.attcollation <> 0 AND a.attcollation <> at.typcollation
                           THEN ' COLLATE ' || (SELECT pg_catalog.quote_ident(cn.nspname) || '.' || pg_catalog.quote_ident(col.collname)
                                                FROM pg_catalog.pg_collation col
                                                JOIN pg_catalog.pg_namespace cn ON cn.oid = col.collnamespace
                                                WHERE col.oid = a.attcollation)
                           ELSE '' END,
                   ', ' ORDER BY a.attnum)
                 FROM pg_catalog.pg_attribute a
                 JOIN pg_catalog.pg_type at ON at.oid = a.atttypid
                 WHERE a.attrelid = t.typrelid AND a.attnum > 0 AND NOT a.attisdropped), ''),
       COALESCE(pg_catalog.obj_description(t.oid, 'pg_type'), '')
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
JOIN pg_catalog.pg_class c ON c.oid = t.typrelid
WHERE t.typtype = 'c' AND c.relkind = 'c' AND n.nspname = ANY($1)
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_type'::regclass
          AND d.objid = t.oid AND d.deptype IN ('e', 'i'))
ORDER BY n.nspname, t.typname`, schemas)
	if err != nil {
		return fmt.Errorf("read composite types: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ns, name, owner, attrs, comment string
		if err := rows.Scan(&ns, &name, &owner, &attrs, &comment); err != nil {
			return err
		}
		c := &model.CompositeType{Schema: ns, Name: name, Owner: owner,
			Definition: "CREATE TYPE " + model.QualifiedName(ns, name) + " AS (" + attrs + ")", Comment: comment}
		if s := byName[ns]; s != nil {
			s.Composites = append(s.Composites, c)
		}
	}
	return rows.Err()
}
