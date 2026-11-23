package introspect

import (
	"context"
	"fmt"

	"github.com/rmnunes/pg-dac/internal/model"
)

// readDefaultPrivileges reads the ALTER DEFAULT PRIVILEGES entries
// (pg_default_acl) scoped IN SCHEMA of a modeled schema, and every
// database-wide entry; the engine decides which database-wide ones a
// comparison manages. The role's own entry is left out, as for object grants.
// An entry that grants nothing (a database-wide one that only took PUBLIC's
// EXECUTE away, for instance) is kept: its existence is the difference.
func readDefaultPrivileges(ctx context.Context, q Querier, schemas []string) ([]*model.DefaultPrivilege, error) {
	rows, err := q.Query(ctx, `
SELECT pg_catalog.pg_get_userbyid(d.defaclrole), COALESCE(n.nspname, ''), d.defaclobjtype::text,
       x.grantee IS NOT NULL AND x.grantee <> d.defaclrole,
       COALESCE(r.rolname, ''), COALESCE(x.privilege_type, ''), COALESCE(x.is_grantable, false)
FROM pg_catalog.pg_default_acl d
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = d.defaclnamespace
LEFT JOIN LATERAL pg_catalog.aclexplode(d.defaclacl) x ON true
LEFT JOIN pg_catalog.pg_roles r ON r.oid = x.grantee
WHERE d.defaclnamespace = 0 OR n.nspname = ANY($1)
ORDER BY 1, 2, 3, 5, 6`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read default privileges: %w", err)
	}
	defer rows.Close()

	var out []*model.DefaultPrivilege
	entries := map[string]*model.DefaultPrivilege{}
	grantIdx := map[string]map[string]int{}
	for rows.Next() {
		var role, schema, objtype, grantee, priv string
		var counted, grantable bool
		if err := rows.Scan(&role, &schema, &objtype, &counted, &grantee, &priv, &grantable); err != nil {
			return nil, err
		}
		typeName, ok := model.DefaultPrivilegeObjectTypes[objtype]
		if !ok {
			continue // an object type pg-dac does not model (large objects)
		}
		key := role + "\x1f" + schema + "\x1f" + typeName
		dp := entries[key]
		if dp == nil {
			dp = &model.DefaultPrivilege{Role: role, Schema: schema, ObjectType: typeName}
			entries[key] = dp
			grantIdx[key] = map[string]int{}
			out = append(out, dp)
		}
		if !counted {
			continue // no grants, or the role's own entry
		}
		i, ok := grantIdx[key][grantee]
		if !ok {
			dp.Grants = append(dp.Grants, model.Grant{Grantee: grantee})
			i = len(dp.Grants) - 1
			grantIdx[key][grantee] = i
		}
		dp.Grants[i].Privileges = append(dp.Grants[i].Privileges, model.Privilege{Type: priv, Grantable: grantable})
	}
	return out, rows.Err()
}
