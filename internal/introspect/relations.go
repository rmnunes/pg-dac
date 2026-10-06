package introspect

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/model"
)

func tkey(schema, name string) string { return schema + "\x1f" + name }

// readCollations introspects user-defined collations. The ICU locale column was
// renamed across versions (colliculocale in PG15/16, colllocale in PG17+), and
// the ICU rules column (collicurules) arrived in PG16 — so the query is built
// against the running major version.
func readCollations(ctx context.Context, q Querier, schemas []string, major int, byName map[string]*model.Schema) error {
	localeCol := "NULL::text"
	if major >= 17 {
		localeCol = "c.colllocale"
	} else if major >= 15 {
		localeCol = "c.colliculocale"
	}
	rulesCol := "NULL::text"
	if major >= 16 {
		rulesCol = "c.collicurules"
	}
	sql := fmt.Sprintf(`
SELECT n.nspname, c.collname,
       pg_catalog.pg_get_userbyid(c.collowner),
       c.collprovider::text,
       c.collcollate, c.collctype, %s, %s,
       c.collisdeterministic,
       COALESCE(pg_catalog.obj_description(c.oid, 'pg_collation'), '')
FROM pg_catalog.pg_collation c
JOIN pg_catalog.pg_namespace n ON n.oid = c.collnamespace
WHERE n.nspname = ANY($1)
  AND c.collprovider <> 'd'   -- skip the 'default' (database default) pseudo-collation
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_collation'::regclass
          AND d.objid = c.oid AND d.deptype IN ('e', 'i'))
ORDER BY n.nspname, c.collname`, localeCol, rulesCol)

	rows, err := q.Query(ctx, sql, schemas)
	if err != nil {
		return fmt.Errorf("read collations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ns, name, owner, prov, comment string
		var collate, ctype, locale, rules *string
		var det bool
		if err := rows.Scan(&ns, &name, &owner, &prov, &collate, &ctype, &locale, &rules, &det, &comment); err != nil {
			return err
		}
		col := &model.Collation{
			Schema:     ns,
			Name:       name,
			Owner:      owner,
			Comment:    comment,
			Definition: buildCollationDef(ns, name, prov, collate, ctype, locale, rules, det),
		}
		if s := byName[ns]; s != nil {
			s.Collations = append(s.Collations, col)
		}
	}
	return rows.Err()
}

// buildCollationDef reconstructs a CREATE COLLATION statement (PostgreSQL has no
// pg_get_collationdef) across the libc / icu / builtin providers.
func buildCollationDef(ns, name, prov string, collate, ctype, locale, rules *string, det bool) string {
	var opts []string
	switch prov {
	case "i": // icu
		opts = append(opts, "provider = icu")
		loc := locale
		if loc == nil {
			loc = collate // PG14 stored the ICU locale in collcollate
		}
		if loc != nil && *loc != "" {
			opts = append(opts, "locale = "+model.QuoteLiteral(*loc))
		}
		if rules != nil && *rules != "" {
			opts = append(opts, "rules = "+model.QuoteLiteral(*rules))
		}
	case "b": // builtin (PG17+)
		opts = append(opts, "provider = builtin")
		if locale != nil && *locale != "" {
			opts = append(opts, "locale = "+model.QuoteLiteral(*locale))
		}
	default: // 'c' libc
		opts = append(opts, "provider = libc")
		if collate != nil && *collate != "" {
			opts = append(opts, "lc_collate = "+model.QuoteLiteral(*collate))
		}
		if ctype != nil && *ctype != "" {
			opts = append(opts, "lc_ctype = "+model.QuoteLiteral(*ctype))
		}
	}
	if !det {
		opts = append(opts, "deterministic = false")
	}
	return "CREATE COLLATION " + model.QualifiedName(ns, name) + " (" + strings.Join(opts, ", ") + ")"
}

func readSequences(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname,
       pg_catalog.pg_get_userbyid(c.relowner),
       COALESCE(pg_catalog.obj_description(c.oid, 'pg_class'), ''),
       pg_catalog.format_type(s.seqtypid, NULL),
       s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,
       dep.rschema, dep.rtable, dep.rcol
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_sequence s ON s.seqrelid = c.oid
LEFT JOIN LATERAL (
    SELECT rn.nspname AS rschema, rc.relname AS rtable, a.attname AS rcol
    FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_class rc ON rc.oid = d.refobjid
    JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
    JOIN pg_catalog.pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
    WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid
      AND d.refclassid = 'pg_catalog.pg_class'::regclass
      AND d.deptype IN ('a', 'i') AND d.refobjsubid > 0
    LIMIT 1
) dep ON true
WHERE c.relkind = 'S' AND n.nspname = ANY($1)
ORDER BY n.nspname, c.relname`, schemas)
	if err != nil {
		return fmt.Errorf("read sequences: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		s := &model.Sequence{}
		var rschema, rtable, rcol *string
		if err := rows.Scan(&s.Schema, &s.Name, &s.Owner, &s.Comment, &s.DataType,
			&s.Start, &s.Increment, &s.MinValue, &s.MaxValue, &s.Cache, &s.Cycle,
			&rschema, &rtable, &rcol); err != nil {
			return err
		}
		if rtable != nil && rcol != nil {
			s.OwnedBy = &model.ColumnRef{Schema: deref(rschema), Table: *rtable, Column: *rcol}
		}
		if sc := byName[s.Schema]; sc != nil {
			sc.Sequences = append(sc.Sequences, s)
		}
	}
	return rows.Err()
}

func readTables(ctx context.Context, q Querier, schemas []string, major int, byName map[string]*model.Schema) (map[string]*model.Table, error) {
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname,
       pg_catalog.pg_get_userbyid(c.relowner),
       COALESCE(pg_catalog.obj_description(c.oid, 'pg_class'), ''),
       c.relpersistence::text,
       c.relrowsecurity, c.relforcerowsecurity,
       COALESCE(pg_catalog.pg_get_partkeydef(c.oid), ''),
       c.relispartition,
       pn.nspname, pc.relname,
       COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), ''),
       COALESCE(c.reloptions, '{}'::text[]),
       COALESCE(tc.reloptions, '{}'::text[])
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_inherits ih ON ih.inhrelid = c.oid AND c.relispartition
LEFT JOIN pg_catalog.pg_class pc ON pc.oid = ih.inhparent
LEFT JOIN pg_catalog.pg_namespace pn ON pn.oid = pc.relnamespace
LEFT JOIN pg_catalog.pg_class tc ON tc.oid = c.reltoastrelid
WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1)
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_class'::regclass
          AND d.objid = c.oid AND d.deptype = 'e')
ORDER BY n.nspname, c.relname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read tables: %w", err)
	}
	defer rows.Close()

	tables := map[string]*model.Table{}
	for rows.Next() {
		t := &model.Table{}
		var persist, partKey, bound string
		var isPartition bool
		var pschema, ptable *string
		var relopts, toastopts []string
		if err := rows.Scan(&t.Schema, &t.Name, &t.Owner, &t.Comment, &persist, &t.RLSEnabled, &t.RLSForced,
			&partKey, &isPartition, &pschema, &ptable, &bound, &relopts, &toastopts); err != nil {
			return nil, err
		}
		t.Persistence = persistence(persist)
		if partKey != "" {
			t.PartitionKey = partKey
			t.PartitionStrategy = strings.ToLower(strings.Fields(partKey)[0])
		}
		if isPartition && ptable != nil {
			t.PartitionOf = &model.PartitionParent{Schema: deref(pschema), Table: *ptable, Bound: bound}
		}
		t.RelOptions = parseRelOptions(relopts, "")
		for k, v := range parseRelOptions(toastopts, "toast.") {
			if t.RelOptions == nil {
				t.RelOptions = map[string]string{}
			}
			t.RelOptions[k] = v
		}
		tables[tkey(t.Schema, t.Name)] = t
		if sc := byName[t.Schema]; sc != nil {
			sc.Tables = append(sc.Tables, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := readColumns(ctx, q, schemas, major, tables); err != nil {
		return nil, err
	}
	if err := readConstraints(ctx, q, schemas, tables); err != nil {
		return nil, err
	}
	if err := readPolicies(ctx, q, schemas, tables); err != nil {
		return nil, err
	}
	return tables, nil
}

// readTriggers reads user triggers on tables and views (INSTEAD OF). Triggers a
// partition inherits from its parent (tgparentid, PG13+) are the parent's and
// are skipped, as are constraint-backed internal triggers.
func readTriggers(ctx context.Context, q Querier, schemas []string, major int, tables map[string]*model.Table, views map[string]*model.View) error {
	inherited := ""
	if major >= 13 {
		inherited = "AND tg.tgparentid = 0"
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
SELECT n.nspname, c.relname, c.relkind::text, tg.tgname,
       pg_catalog.pg_get_triggerdef(tg.oid),
       tg.tgenabled::text,
       COALESCE(pg_catalog.obj_description(tg.oid, 'pg_trigger'), ''),
       fn.nspname,
       fp.proname || '(' || pg_catalog.pg_get_function_identity_arguments(fp.oid) || ')'
FROM pg_catalog.pg_trigger tg
JOIN pg_catalog.pg_class c ON c.oid = tg.tgrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_proc fp ON fp.oid = tg.tgfoid
JOIN pg_catalog.pg_namespace fn ON fn.oid = fp.pronamespace
WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'v')
  AND NOT tg.tgisinternal     -- skip FK/constraint-backed internal triggers
  %s
ORDER BY n.nspname, c.relname, tg.tgname`, inherited), schemas)
	if err != nil {
		return fmt.Errorf("read triggers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, rel, relkind, enabled, fnSchema, fnSig string
		tr := &model.Trigger{}
		if err := rows.Scan(&schema, &rel, &relkind, &tr.Name, &tr.Definition, &enabled, &tr.Comment, &fnSchema, &fnSig); err != nil {
			return err
		}
		tr.Definition = nlnorm(tr.Definition)
		tr.Table = rel
		tr.Enabled = triggerEnabled(enabled)
		tr.DependsOn = []model.ObjRef{{Kind: model.KindFunction, Schema: fnSchema, Name: fnSig}}
		if relkind == "v" {
			if v := views[tkey(schema, rel)]; v != nil {
				v.Triggers = append(v.Triggers, tr)
			}
			continue
		}
		if t := tables[tkey(schema, rel)]; t != nil {
			t.Triggers = append(t.Triggers, tr)
		}
	}
	return rows.Err()
}

func readPolicies(ctx context.Context, q Querier, schemas []string, tables map[string]*model.Table) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname, pol.polname, pol.polpermissive,
       CASE pol.polcmd WHEN '*' THEN 'ALL' WHEN 'r' THEN 'SELECT' WHEN 'a' THEN 'INSERT'
                       WHEN 'w' THEN 'UPDATE' WHEN 'd' THEN 'DELETE' ELSE pol.polcmd::text END,
       COALESCE(
         (SELECT array_agg(pg_catalog.pg_get_userbyid(r) ORDER BY r) FROM unnest(pol.polroles) AS r
          WHERE r <> 0),
         '{}'),
       (pol.polroles = '{0}'),   -- applies to PUBLIC
       COALESCE(pg_catalog.pg_get_expr(pol.polqual, pol.polrelid), ''),
       COALESCE(pg_catalog.pg_get_expr(pol.polwithcheck, pol.polrelid), ''),
       COALESCE(pg_catalog.obj_description(pol.oid, 'pg_policy'), '')
FROM pg_catalog.pg_policy pol
JOIN pg_catalog.pg_class c ON c.oid = pol.polrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1)
ORDER BY n.nspname, c.relname, pol.polname`, schemas)
	if err != nil {
		return fmt.Errorf("read policies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table string
		var public bool
		p := &model.Policy{}
		if err := rows.Scan(&schema, &table, &p.Name, &p.Permissive, &p.Command,
			&p.Roles, &public, &p.Using, &p.WithCheck, &p.Comment); err != nil {
			return err
		}
		if public {
			p.Roles = nil // PUBLIC is the implicit default; omit explicit role list
		}
		p.Table = table
		p.Using, p.WithCheck = nlnorm(p.Using), nlnorm(p.WithCheck)
		if t := tables[tkey(schema, table)]; t != nil {
			t.Policies = append(t.Policies, p)
		}
	}
	return rows.Err()
}

// readColumns reads table columns. A partition's columns are inherited from its
// parent and are not modeled on the partition. Storage attributes are recorded
// only when they differ from the type's default so they never produce noise.
func readColumns(ctx context.Context, q Querier, schemas []string, major int, tables map[string]*model.Table) error {
	compressionCol := "''::text"
	if major >= 14 {
		compressionCol = "a.attcompression::text"
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attnum, a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       a.attnotnull,
       COALESCE(pg_catalog.pg_get_expr(ad.adbin, ad.adrelid), ''),
       a.attidentity::text,
       a.attgenerated::text,
       CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation
            THEN (SELECT pg_catalog.quote_ident(cn.nspname) || '.' || pg_catalog.quote_ident(col.collname)
                  FROM pg_catalog.pg_collation col
                  JOIN pg_catalog.pg_namespace cn ON cn.oid = col.collnamespace
                  WHERE col.oid = a.attcollation)
            ELSE '' END,
       COALESCE(pg_catalog.col_description(c.oid, a.attnum), ''),
       CASE WHEN a.attstorage <> t.typstorage THEN a.attstorage::text ELSE '' END,
       %s,
       COALESCE(a.attstattarget, -1)::int
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_catalog.pg_attrdef ad ON ad.adrelid = c.oid AND ad.adnum = a.attnum
WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND n.nspname = ANY($1)
ORDER BY n.nspname, c.relname, a.attnum`, compressionCol), schemas)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table, identity, generated, def, storage, compression string
		var stats int
		c := &model.Column{}
		if err := rows.Scan(&schema, &table, &c.AttNum, &c.Name, &c.Type, &c.NotNull,
			&def, &identity, &generated, &c.Collation, &c.Comment, &storage, &compression, &stats); err != nil {
			return err
		}
		switch generated {
		case "s":
			c.Generated, c.GeneratedExpr = "stored", def
		case "v":
			c.Generated, c.GeneratedExpr = "virtual", def
		default:
			switch identity {
			case "a":
				c.Identity = "always"
			case "d":
				c.Identity = "byDefault"
			default:
				c.Default = def
			}
		}
		c.Storage = storageName(storage)
		c.Compression = compressionName(compression)
		if stats >= 0 {
			n := stats
			c.StatsTarget = &n
		}
		if t := tables[tkey(schema, table)]; t != nil {
			t.Columns = append(t.Columns, c)
		}
	}
	return rows.Err()
}

// readConstraints reads table constraints. Constraints a partition inherits
// from its parent (conislocal = false) belong to the parent and are skipped.
func readConstraints(ctx context.Context, q Querier, schemas []string, tables map[string]*model.Table) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname, con.conname, con.contype::text, con.convalidated,
       pg_catalog.pg_get_constraintdef(con.oid),
       con.condeferrable, con.condeferred,
       (SELECT array_agg(a.attname ORDER BY x.ord)
          FROM unnest(con.conkey) WITH ORDINALITY x(attnum, ord)
          JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = x.attnum),
       rn.nspname, rc.relname,
       (SELECT array_agg(a.attname ORDER BY x.ord)
          FROM unnest(con.confkey) WITH ORDINALITY x(attnum, ord)
          JOIN pg_catalog.pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = x.attnum),
       COALESCE(pg_catalog.obj_description(con.oid, 'pg_constraint'), '')
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_class rc ON rc.oid = con.confrelid
LEFT JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
WHERE con.contype IN ('p', 'f', 'u', 'c', 'x')
  AND con.conislocal
  AND n.nspname = ANY($1) AND c.relkind IN ('r', 'p')
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_constraint'::regclass
          AND d.objid = con.oid AND d.deptype = 'e')
ORDER BY n.nspname, c.relname, con.conname`, schemas)
	if err != nil {
		return fmt.Errorf("read constraints: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, table, contype string
		var refSchema, refTable *string
		con := &model.Constraint{}
		if err := rows.Scan(&schema, &table, &con.Name, &contype, &con.Validated,
			&con.Definition, &con.Deferrable, &con.InitiallyDeferred,
			&con.Columns, &refSchema, &refTable, &con.RefColumns, &con.Comment); err != nil {
			return err
		}
		con.Type = constraintType(contype)
		// pg_get_constraintdef appends " NOT VALID" for unvalidated constraints;
		// that state is carried by Validated, so strip it from the definition (the
		// generator re-adds it, and an equal body must compare equal).
		con.Definition = strings.TrimSuffix(nlnorm(con.Definition), " NOT VALID")
		con.RefSchema, con.RefTable = deref(refSchema), deref(refTable)
		if t := tables[tkey(schema, table)]; t != nil {
			t.Constraints = append(t.Constraints, con)
		}
	}
	return rows.Err()
}

// readIndexes reads stand-alone indexes on tables and materialized views.
// Constraint-backed indexes are modeled with their constraint; indexes a
// partition inherits from a partitioned index (pg_inherits) are the parent's.
func readIndexes(ctx context.Context, q Querier, schemas []string, tables map[string]*model.Table, mviews map[string]*model.MaterializedView) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname, c.relkind::text, ic.relname,
       pg_catalog.pg_get_indexdef(i.indexrelid),
       i.indisunique, am.amname,
       COALESCE(pg_catalog.obj_description(i.indexrelid, 'pg_class'), '')
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = ic.relam
WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'm')
  AND NOT i.indisprimary
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint con
        WHERE con.conindid = i.indexrelid AND con.contype IN ('p', 'u', 'x'))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits ih WHERE ih.inhrelid = i.indexrelid)
  AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_class'::regclass
          AND d.objid = i.indexrelid AND d.deptype = 'e')
ORDER BY n.nspname, c.relname, ic.relname`, schemas)
	if err != nil {
		return fmt.Errorf("read indexes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, rel, relkind string
		idx := &model.Index{}
		if err := rows.Scan(&schema, &rel, &relkind, &idx.Name, &idx.Definition, &idx.IsUnique, &idx.Method, &idx.Comment); err != nil {
			return err
		}
		idx.Definition = nlnorm(idx.Definition)
		idx.Table = rel
		if relkind == "m" {
			if mv := mviews[tkey(schema, rel)]; mv != nil {
				mv.Indexes = append(mv.Indexes, idx)
			}
			continue
		}
		if t := tables[tkey(schema, rel)]; t != nil {
			t.Indexes = append(t.Indexes, idx)
		}
	}
	return rows.Err()
}

func readViews(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) (map[string]*model.View, error) {
	deps, err := readViewDeps(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	cols, err := readViewColumns(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	colDeps, err := readViewColumnDeps(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname,
       pg_catalog.pg_get_userbyid(c.relowner),
       COALESCE(pg_catalog.obj_description(c.oid, 'pg_class'), ''),
       pg_catalog.pg_get_viewdef(c.oid),
       COALESCE(c.reloptions, '{}'::text[])
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'v' AND n.nspname = ANY($1)
ORDER BY n.nspname, c.relname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read views: %w", err)
	}
	defer rows.Close()

	views := map[string]*model.View{}
	for rows.Next() {
		v := &model.View{}
		var opts []string
		if err := rows.Scan(&v.Schema, &v.Name, &v.Owner, &v.Comment, &v.Definition, &opts); err != nil {
			return nil, err
		}
		v.Definition = trimDef(v.Definition)
		v.DependsOn = deps[tkey(v.Schema, v.Name)]
		v.Columns = cols[tkey(v.Schema, v.Name)]
		v.ColumnDeps = colDeps[tkey(v.Schema, v.Name)]
		for k, val := range parseRelOptions(opts, "") {
			switch k {
			case "check_option":
				v.CheckOption = strings.ToLower(val)
			case "security_barrier":
				v.SecurityBarrier = isTrue(val)
			case "security_invoker":
				v.SecurityInvoker = isTrue(val)
			}
		}
		views[tkey(v.Schema, v.Name)] = v
		if sc := byName[v.Schema]; sc != nil {
			sc.Views = append(sc.Views, v)
		}
	}
	return views, rows.Err()
}

func readMatViews(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema) (map[string]*model.MaterializedView, error) {
	deps, err := readViewDeps(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	colDeps, err := readViewColumnDeps(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname,
       pg_catalog.pg_get_userbyid(c.relowner),
       COALESCE(pg_catalog.obj_description(c.oid, 'pg_class'), ''),
       pg_catalog.pg_get_viewdef(c.oid),
       c.relispopulated
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'm' AND n.nspname = ANY($1)
ORDER BY n.nspname, c.relname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read matviews: %w", err)
	}
	defer rows.Close()

	mviews := map[string]*model.MaterializedView{}
	for rows.Next() {
		m := &model.MaterializedView{}
		if err := rows.Scan(&m.Schema, &m.Name, &m.Owner, &m.Comment, &m.Definition, &m.WithData); err != nil {
			return nil, err
		}
		m.Definition = trimDef(m.Definition)
		m.DependsOn = deps[tkey(m.Schema, m.Name)]
		m.ColumnDeps = colDeps[tkey(m.Schema, m.Name)]
		mviews[tkey(m.Schema, m.Name)] = m
		if sc := byName[m.Schema]; sc != nil {
			sc.MatViews = append(sc.MatViews, m)
		}
	}
	return mviews, rows.Err()
}

// readViewColumns returns each view's output columns (name + canonical type)
// in position order, keyed by tkey(schema,name).
func readViewColumns(ctx context.Context, q Querier, schemas []string) (map[string][]model.ViewColumn, error) {
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.relkind = 'v' AND n.nspname = ANY($1)
ORDER BY n.nspname, c.relname, a.attnum`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read view columns: %w", err)
	}
	defer rows.Close()

	out := map[string][]model.ViewColumn{}
	for rows.Next() {
		var vschema, vname string
		var col model.ViewColumn
		if err := rows.Scan(&vschema, &vname, &col.Name, &col.Type); err != nil {
			return nil, err
		}
		out[tkey(vschema, vname)] = append(out[tkey(vschema, vname)], col)
	}
	return out, rows.Err()
}

// readViewColumnDeps returns, for each view/matview, the specific columns of
// other relations it references (pg_depend rows with refobjsubid > 0). These
// are the columns whose type change or drop PostgreSQL refuses while the view
// exists, so the engine knows exactly which views to recreate around them.
func readViewColumnDeps(ctx context.Context, q Querier, schemas []string) (map[string][]model.ColumnRef, error) {
	rows, err := q.Query(ctx, `
SELECT DISTINCT vn.nspname, vc.relname, rn.nspname, rc.relname, a.attname, a.attnum
FROM pg_catalog.pg_rewrite rw
JOIN pg_catalog.pg_class vc ON vc.oid = rw.ev_class
JOIN pg_catalog.pg_namespace vn ON vn.oid = vc.relnamespace
JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_rewrite'::regclass
     AND d.objid = rw.oid AND d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjsubid > 0
JOIN pg_catalog.pg_class rc ON rc.oid = d.refobjid
JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = rc.oid AND a.attnum = d.refobjsubid
WHERE vc.relkind IN ('v', 'm') AND vn.nspname = ANY($1) AND rc.oid <> vc.oid
ORDER BY 1, 2, 3, 4, 6`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read view column deps: %w", err)
	}
	defer rows.Close()

	out := map[string][]model.ColumnRef{}
	for rows.Next() {
		var vschema, vname, rschema, rname, col string
		var attnum int
		if err := rows.Scan(&vschema, &vname, &rschema, &rname, &col, &attnum); err != nil {
			return nil, err
		}
		out[tkey(vschema, vname)] = append(out[tkey(vschema, vname)], model.ColumnRef{Schema: rschema, Table: rname, Column: col})
	}
	return out, rows.Err()
}

// readViewDeps returns, for each view/matview keyed by tkey(schema,name), the
// relations and functions it references, derived from pg_rewrite + pg_depend.
// Function references matter for ordering: a function may itself have to wait
// for a later object (e.g. the view whose row type it returns), so a view that
// calls it cannot rely on the coarse "functions before views" band alone.
func readViewDeps(ctx context.Context, q Querier, schemas []string) (map[string][]model.ObjRef, error) {
	rows, err := q.Query(ctx, `
SELECT DISTINCT vn.nspname, vc.relname, rn.nspname, rc.relname, rc.relkind::text
FROM pg_catalog.pg_rewrite rw
JOIN pg_catalog.pg_class vc ON vc.oid = rw.ev_class
JOIN pg_catalog.pg_namespace vn ON vn.oid = vc.relnamespace
JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_rewrite'::regclass
     AND d.objid = rw.oid AND d.refclassid = 'pg_catalog.pg_class'::regclass
JOIN pg_catalog.pg_class rc ON rc.oid = d.refobjid
JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
WHERE vc.relkind IN ('v', 'm') AND vn.nspname = ANY($1)
  AND rc.oid <> vc.oid AND rc.relkind IN ('r', 'p', 'v', 'm')
UNION
SELECT DISTINCT vn.nspname, vc.relname, fn.nspname,
       fp.proname || '(' || pg_catalog.pg_get_function_identity_arguments(fp.oid) || ')', 'f'
FROM pg_catalog.pg_rewrite rw
JOIN pg_catalog.pg_class vc ON vc.oid = rw.ev_class
JOIN pg_catalog.pg_namespace vn ON vn.oid = vc.relnamespace
JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_rewrite'::regclass
     AND d.objid = rw.oid AND d.refclassid = 'pg_catalog.pg_proc'::regclass
JOIN pg_catalog.pg_proc fp ON fp.oid = d.refobjid
JOIN pg_catalog.pg_namespace fn ON fn.oid = fp.pronamespace
WHERE vc.relkind IN ('v', 'm') AND vn.nspname = ANY($1)
ORDER BY 1, 2, 5, 3, 4`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read view deps: %w", err)
	}
	defer rows.Close()

	out := map[string][]model.ObjRef{}
	for rows.Next() {
		var vschema, vname, rschema, rname, rkind string
		if err := rows.Scan(&vschema, &vname, &rschema, &rname, &rkind); err != nil {
			return nil, err
		}
		out[tkey(vschema, vname)] = append(out[tkey(vschema, vname)], model.ObjRef{
			Kind: relkindToObjKind(rkind), Schema: rschema, Name: rname,
		})
	}
	return out, rows.Err()
}

// --- small mapping helpers ---

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func trimDef(s string) string { return strings.TrimRight(strings.TrimSpace(nlnorm(s)), ";") }

// nlnorm normalizes line endings to LF so a definition that differs only by CRLF
// vs LF (e.g. a function body deployed from Windows vs from Linux CI) is not
// reported as a change. PostgreSQL stores function bodies verbatim, so this is
// the one bit of whitespace the canonical-model approach can't otherwise smooth.
func nlnorm(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func persistence(p string) string {
	switch p {
	case "u":
		return "unlogged"
	case "t":
		return "temporary"
	default:
		return ""
	}
}

func constraintType(t string) model.ConstraintType {
	switch t {
	case "p":
		return model.ConstraintPrimaryKey
	case "f":
		return model.ConstraintForeignKey
	case "u":
		return model.ConstraintUnique
	case "c":
		return model.ConstraintCheck
	case "x":
		return model.ConstraintExclude
	}
	return model.ConstraintType(t)
}

func relkindToObjKind(k string) model.ObjKind {
	switch k {
	case "v":
		return model.KindView
	case "m":
		return model.KindMatView
	case "S":
		return model.KindSequence
	case "e":
		return model.KindEnum
	case "f":
		return model.KindFunction
	default:
		return model.KindTable
	}
}

var _ = pgx.ErrNoRows

// parseRelOptions turns a reloptions array ("fillfactor=70", ...) into a map,
// prefixing keys (e.g. "toast.") when asked. Returns nil for no options.
func parseRelOptions(opts []string, prefix string) map[string]string {
	var out map[string]string
	for _, o := range opts {
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[prefix+k] = v
	}
	return out
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "true", "on", "1", "yes":
		return true
	}
	return false
}

func storageName(s string) string {
	switch s {
	case "p":
		return "plain"
	case "e":
		return "external"
	case "m":
		return "main"
	case "x":
		return "extended"
	}
	return ""
}

func compressionName(c string) string {
	switch c {
	case "p":
		return "pglz"
	case "l":
		return "lz4"
	}
	return ""
}

func triggerEnabled(e string) string {
	switch e {
	case "D":
		return "disabled"
	case "R":
		return "replica"
	case "A":
		return "always"
	}
	return "" // 'O' — enabled (origin), the default
}
