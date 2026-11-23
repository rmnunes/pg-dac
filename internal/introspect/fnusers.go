package introspect

import (
	"context"
	"fmt"

	"github.com/rmnunes/pg-dac/internal/model"
)

// readFunctionUsers records which user functions a column default, a CHECK
// constraint, an index expression or predicate, or a policy calls (pg_depend,
// deptype 'n'). PostgreSQL refuses to drop a function while any of them
// exists, so the engine drops and restores them around a function it
// recreates, and the planner orders them by these references. Functions in
// pg_catalog and information_schema are never recreated and are left out, as
// are generated columns: their expression cannot be removed and put back
// without rewriting the column.
func readFunctionUsers(ctx context.Context, q Querier, schemas []string, tables map[string]*model.Table, mviews map[string]*model.MaterializedView) error {
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname, x.kind, x.sub, fn.nspname,
       p.proname || '(' || pg_catalog.pg_get_function_identity_arguments(p.oid) || ')'
FROM (
    SELECT 'default' AS kind, ad.adrelid AS relid, a.attname::text AS sub, d.refobjid AS fn
    FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_attrdef ad ON d.classid = 'pg_catalog.pg_attrdef'::regclass AND d.objid = ad.oid
    JOIN pg_catalog.pg_attribute a ON a.attrelid = ad.adrelid AND a.attnum = ad.adnum
    WHERE d.refclassid = 'pg_catalog.pg_proc'::regclass AND d.deptype = 'n'
  UNION ALL
    SELECT 'constraint', con.conrelid, con.conname::text, d.refobjid
    FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_constraint con ON d.classid = 'pg_catalog.pg_constraint'::regclass AND d.objid = con.oid
    WHERE d.refclassid = 'pg_catalog.pg_proc'::regclass AND d.deptype = 'n' AND con.conrelid <> 0
  UNION ALL
    SELECT 'index', i.indrelid, ic.relname::text, d.refobjid
    FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_index i ON d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = i.indexrelid
    JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
    WHERE d.refclassid = 'pg_catalog.pg_proc'::regclass AND d.deptype = 'n'
  UNION ALL
    SELECT 'policy', pol.polrelid, pol.polname::text, d.refobjid
    FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_policy pol ON d.classid = 'pg_catalog.pg_policy'::regclass AND d.objid = pol.oid
    WHERE d.refclassid = 'pg_catalog.pg_proc'::regclass AND d.deptype = 'n'
) x
JOIN pg_catalog.pg_class c ON c.oid = x.relid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_proc p ON p.oid = x.fn
JOIN pg_catalog.pg_namespace fn ON fn.oid = p.pronamespace
WHERE n.nspname = ANY($1) AND fn.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1, 2, 3, 4, 5, 6`, schemas)
	if err != nil {
		return fmt.Errorf("read function dependents: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var schema, rel, kind, sub, fnSchema, fnSig string
		if err := rows.Scan(&schema, &rel, &kind, &sub, &fnSchema, &fnSig); err != nil {
			return err
		}
		ref := model.ObjRef{Kind: model.KindFunction, Schema: fnSchema, Name: fnSig}
		t := tables[tkey(schema, rel)]
		switch kind {
		case "default":
			if t == nil {
				continue
			}
			for _, c := range t.Columns {
				if c.Name == sub && c.Generated == "" {
					c.DefaultDependsOn = append(c.DefaultDependsOn, ref)
				}
			}
		case "constraint":
			if t == nil {
				continue
			}
			for _, con := range t.Constraints {
				if con.Name == sub && con.Type == model.ConstraintCheck {
					con.DependsOn = append(con.DependsOn, ref)
				}
			}
		case "index":
			var indexes []*model.Index
			if t != nil {
				indexes = t.Indexes
			} else if mv := mviews[tkey(schema, rel)]; mv != nil {
				indexes = mv.Indexes
			}
			for _, ix := range indexes {
				if ix.Name == sub {
					ix.DependsOn = append(ix.DependsOn, ref)
				}
			}
		case "policy":
			if t == nil {
				continue
			}
			for _, pol := range t.Policies {
				if pol.Name == sub {
					pol.DependsOn = append(pol.DependsOn, ref)
				}
			}
		}
	}
	return rows.Err()
}
