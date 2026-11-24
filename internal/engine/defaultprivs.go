package engine

import (
	"sort"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// expandDefaultPrivileges adds the ALTER DEFAULT PRIVILEGES grants and revokes
// that bring the target's default-privilege entries to the desired ones.
//
// In scope are the entries IN SCHEMA of a schema the desired state has (and
// the migration keeps), and the database-wide entries of the roles the
// desired state has database-wide entries for or Options.DefaultPrivilegeRoles
// names. Database-wide entries of any other role belong to someone else and
// are left alone.
//
// The comparison is between effective default ACLs: a missing schema entry
// adds nothing, a missing database-wide entry stands for PostgreSQL's built-in
// defaults (EXECUTE on functions and USAGE on types for PUBLIC). An entry the
// desired state has is managed in full, so one that takes PUBLIC's EXECUTE away
// is applied as such; for an entry it does not have, privileges the target
// holds beyond the built-in ones are revoked only under DropObjectsNotInSource,
// like object privileges (DropPermissionsNotInSource). With IgnorePrivileges
// nothing is compared.
func expandDefaultPrivileges(cs *diff.ChangeSet, from, to *model.Database, opts Options) *diff.ChangeSet {
	if to == nil || opts.IgnorePrivileges {
		return cs
	}
	var fromEntries []*model.DefaultPrivilege
	if from != nil {
		fromEntries = from.DefaultPrivileges
	}
	if len(fromEntries) == 0 && len(to.DefaultPrivileges) == 0 {
		return cs
	}

	// Schemas whose entries are compared: in the desired state and not dropped
	// by this migration (a schema's entries go with it).
	droppedSchemas := map[string]bool{}
	createdSchemas := map[string]bool{}
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpDropSchema:
			droppedSchemas[op.Owner.Name] = true
		case diff.OpCreateSchema:
			createdSchemas[op.Owner.Name] = true
		}
	}
	schemaInScope := func(name string) bool {
		return to.Schema(name) != nil && (!droppedSchemas[name] || createdSchemas[name])
	}
	globalRoles := map[string]bool{}
	if opts.Owner != "" {
		globalRoles[opts.Owner] = true
	}
	for _, r := range opts.DefaultPrivilegeRoles {
		globalRoles[r] = true
	}
	for _, dp := range to.DefaultPrivileges {
		if dp.Schema == "" {
			globalRoles[dp.Role] = true
		}
	}
	inScope := func(dp *model.DefaultPrivilege) bool {
		if dp.Schema == "" {
			return globalRoles[dp.Role]
		}
		return schemaInScope(dp.Schema)
	}

	type key struct{ role, schema, objtype string }
	keyOf := func(dp *model.DefaultPrivilege) key { return key{dp.Role, dp.Schema, dp.ObjectType} }
	current := map[key]*model.DefaultPrivilege{}
	desired := map[key]*model.DefaultPrivilege{}
	var keys []key
	seen := map[key]bool{}
	note := func(k key) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for _, dp := range fromEntries {
		if inScope(dp) {
			current[keyOf(dp)] = dp
			note(keyOf(dp))
		}
	}
	for _, dp := range to.DefaultPrivileges {
		if inScope(dp) {
			desired[keyOf(dp)] = dp
			note(keyOf(dp))
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.schema != b.schema {
			return a.schema < b.schema
		}
		if a.role != b.role {
			return a.role < b.role
		}
		return a.objtype < b.objtype
	})

	// effective is the default ACL an entry (or its absence) stands for.
	effective := func(dp *model.DefaultPrivilege, k key) acl {
		switch {
		case dp != nil:
			return aclOf(dp.Grants)
		case k.schema == "":
			return aclOf(model.BuiltinDefaultGrants(k.objtype))
		}
		return acl{}
	}
	var roles map[string]bool
	if from != nil && len(from.Roles) > 0 {
		roles = map[string]bool{}
		for _, r := range from.Roles {
			roles[r] = true
		}
	}
	missing := func(r string) bool { return roles != nil && r != "" && !roles[r] }

	out := &diff.ChangeSet{Ops: append([]diff.Op(nil), cs.Ops...), Warnings: cs.Warnings}
	for _, k := range keys {
		cur, want := current[k], desired[k]
		entry := want
		if entry == nil {
			entry = cur
		}
		ref := entry.Ref()
		have, need := effective(cur, k), effective(want, k)
		for _, g := range grantsNeeded(have, need) {
			g := g
			out.Ops = append(out.Ops, diff.Op{Type: diff.OpGrantDefault, Owner: ref, DefaultPriv: entry, Grant: &g,
				MissingRole: missing(g.Grantee), MissingForRole: missing(k.role)})
		}
		if want != nil || opts.DropObjectsNotInSource {
			for _, g := range revokesNeeded(have, need) {
				g := g
				out.Ops = append(out.Ops, diff.Op{Type: diff.OpRevokeDefault, Owner: ref, DefaultPriv: entry, Grant: &g})
			}
		}
	}
	return out
}
