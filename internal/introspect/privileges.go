package introspect

import (
	"context"
	"fmt"

	"github.com/rmnunes/pg-dac/internal/model"
)

// readPrivileges attaches the effective ACL of every modeled object to the
// model as Grants. Each query explodes COALESCE(acl, acldefault(kind, owner)),
// so an object that was never granted anything reports PostgreSQL's built-in
// defaults (PUBLIC's EXECUTE on routines and USAGE on types, for instance) and
// compares equal to an object whose ACL was materialized by a later GRANT; the
// owner's own entry is excluded on every object because ownership is not
// modeled and the owner holds every privilege implicitly. Grants are attached
// only to objects the model holds, so extension members and other filtered
// objects carry none.
func readPrivileges(ctx context.Context, q Querier, schemas []string, byName map[string]*model.Schema,
	tables map[string]*model.Table, views map[string]*model.View, mviews map[string]*model.MaterializedView) error {
	acc := newACLAccumulator()

	// Relations: tables, partitioned tables, views, materialized views, sequences.
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname, COALESCE(r.rolname, ''), x.privilege_type, x.is_grantable
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(c.relacl,
       pg_catalog.acldefault((CASE WHEN c.relkind = 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))) x
LEFT JOIN pg_catalog.pg_roles r ON r.oid = x.grantee
WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'v', 'm', 'S') AND x.grantee <> c.relowner
ORDER BY 1, 2, 3, 4`, schemas)
	if err != nil {
		return fmt.Errorf("read relation privileges: %w", err)
	}
	if err := acc.scan(rows, 2); err != nil {
		return err
	}
	for k, grants := range acc.flush() {
		schema, name := splitTkey(k)
		switch {
		case tables[tkey(schema, name)] != nil:
			tables[tkey(schema, name)].Grants = grants
		case views[tkey(schema, name)] != nil:
			views[tkey(schema, name)].Grants = grants
		case mviews[tkey(schema, name)] != nil:
			mviews[tkey(schema, name)].Grants = grants
		default:
			if s := byName[schema]; s != nil {
				for _, seq := range s.Sequences {
					if seq.Name == name {
						seq.Grants = grants
					}
				}
			}
		}
	}

	// Column privileges on tables (a partition's columns are its parent's).
	rows, err = q.Query(ctx, `
SELECT n.nspname, c.relname, a.attname, COALESCE(r.rolname, ''), x.privilege_type, x.is_grantable
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(a.attacl) x
LEFT JOIN pg_catalog.pg_roles r ON r.oid = x.grantee
WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p') AND NOT c.relispartition
  AND a.attnum > 0 AND NOT a.attisdropped AND a.attacl IS NOT NULL AND x.grantee <> c.relowner
ORDER BY 1, 2, 3, 4, 5`, schemas)
	if err != nil {
		return fmt.Errorf("read column privileges: %w", err)
	}
	if err := acc.scan(rows, 3); err != nil {
		return err
	}
	for k, grants := range acc.flush() {
		schema, rest := splitTkey(k)
		table, col := splitTkey(rest)
		if t := tables[tkey(schema, table)]; t != nil {
			if c := t.Column(col); c != nil {
				c.Grants = grants
			}
		}
	}

	// Routines.
	rows, err = q.Query(ctx, `
SELECT n.nspname, p.proname || '(' || pg_catalog.pg_get_function_identity_arguments(p.oid) || ')',
       COALESCE(r.rolname, ''), x.privilege_type, x.is_grantable
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))) x
LEFT JOIN pg_catalog.pg_roles r ON r.oid = x.grantee
WHERE n.nspname = ANY($1) AND p.prokind IN ('f', 'p') AND x.grantee <> p.proowner
ORDER BY 1, 2, 3, 4`, schemas)
	if err != nil {
		return fmt.Errorf("read routine privileges: %w", err)
	}
	if err := acc.scan(rows, 2); err != nil {
		return err
	}
	for k, grants := range acc.flush() {
		schema, sig := splitTkey(k)
		if s := byName[schema]; s != nil {
			for _, f := range s.Functions {
				if f.Signature() == sig {
					f.Grants = grants
				}
			}
		}
	}

	// Schemas.
	rows, err = q.Query(ctx, `
SELECT n.nspname, '', COALESCE(r.rolname, ''), x.privilege_type, x.is_grantable
FROM pg_catalog.pg_namespace n
CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) x
LEFT JOIN pg_catalog.pg_roles r ON r.oid = x.grantee
WHERE n.nspname = ANY($1) AND x.grantee <> n.nspowner
ORDER BY 1, 3, 4`, schemas)
	if err != nil {
		return fmt.Errorf("read schema privileges: %w", err)
	}
	if err := acc.scan(rows, 2); err != nil {
		return err
	}
	for k, grants := range acc.flush() {
		schema, _ := splitTkey(k)
		if s := byName[schema]; s != nil {
			s.Grants = grants
		}
	}

	// Types: enums, domains, stand-alone composites (a table's row type is
	// typtype 'c' too, but no composite object exists for it, so it is skipped).
	rows, err = q.Query(ctx, `
SELECT n.nspname, t.typname, COALESCE(r.rolname, ''), x.privilege_type, x.is_grantable
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(t.typacl, pg_catalog.acldefault('T', t.typowner))) x
LEFT JOIN pg_catalog.pg_roles r ON r.oid = x.grantee
WHERE n.nspname = ANY($1) AND t.typtype IN ('e', 'd', 'c') AND x.grantee <> t.typowner
ORDER BY 1, 2, 3, 4`, schemas)
	if err != nil {
		return fmt.Errorf("read type privileges: %w", err)
	}
	if err := acc.scan(rows, 2); err != nil {
		return err
	}
	for k, grants := range acc.flush() {
		schema, name := splitTkey(k)
		s := byName[schema]
		if s == nil {
			continue
		}
		for _, e := range s.Enums {
			if e.Name == name {
				e.Grants = grants
			}
		}
		for _, d := range s.Domains {
			if d.Name == name {
				d.Grants = grants
			}
		}
		for _, c := range s.Composites {
			if c.Name == name {
				c.Grants = grants
			}
		}
	}
	return nil
}

// readRoles returns every role name in the cluster.
func readRoles(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT rolname FROM pg_catalog.pg_roles ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("read roles: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// aclAccumulator groups exploded ACL rows (object key, grantee, privilege,
// grantable) into per-object Grant lists.
type aclAccumulator struct {
	order  []string
	byObj  map[string]map[string]*model.Grant
	gorder map[string][]string
}

func newACLAccumulator() *aclAccumulator {
	return &aclAccumulator{byObj: map[string]map[string]*model.Grant{}, gorder: map[string][]string{}}
}

// scan consumes rows whose first keyParts columns identify the object (joined
// with tkey), followed by grantee, privilege_type, is_grantable.
func (a *aclAccumulator) scan(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}, keyParts int) error {
	defer rows.Close()
	for rows.Next() {
		parts := make([]string, keyParts)
		dest := make([]any, 0, keyParts+3)
		for i := range parts {
			dest = append(dest, &parts[i])
		}
		var grantee, priv string
		var grantable bool
		dest = append(dest, &grantee, &priv, &grantable)
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		key := parts[0]
		for _, p := range parts[1:] {
			key = tkey(key, p)
		}
		a.add(key, grantee, model.Privilege{Type: priv, Grantable: grantable})
	}
	return rows.Err()
}

func (a *aclAccumulator) add(key, grantee string, p model.Privilege) {
	m, ok := a.byObj[key]
	if !ok {
		m = map[string]*model.Grant{}
		a.byObj[key] = m
		a.order = append(a.order, key)
	}
	g, ok := m[grantee]
	if !ok {
		g = &model.Grant{Grantee: grantee}
		m[grantee] = g
		a.gorder[key] = append(a.gorder[key], grantee)
	}
	g.Privileges = append(g.Privileges, p)
}

// flush returns the accumulated grants per object key (sorted) and resets.
func (a *aclAccumulator) flush() map[string][]model.Grant {
	out := map[string][]model.Grant{}
	for _, key := range a.order {
		var gs []model.Grant
		for _, grantee := range a.gorder[key] {
			gs = append(gs, *a.byObj[key][grantee])
		}
		model.SortGrants(gs)
		out[key] = gs
	}
	a.order, a.byObj, a.gorder = nil, map[string]map[string]*model.Grant{}, map[string][]string{}
	return out
}

// splitTkey undoes one level of tkey.
func splitTkey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '\x1f' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
