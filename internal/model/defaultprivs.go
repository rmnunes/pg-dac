package model

import "sort"

// DefaultPrivilege models one pg_default_acl entry: the privileges an object
// of ObjectType receives when Role creates it — in Schema, or anywhere in the
// database when Schema is "" (ALTER DEFAULT PRIVILEGES without IN SCHEMA).
//
// Grants exclude Role's own entry: the creator owns what it creates and holds
// every privilege implicitly, as for object grants. A database-wide entry
// replaces PostgreSQL's built-in defaults for its object type, so an entry
// with no grants records that PUBLIC lost its EXECUTE on functions or its
// USAGE on types; a schema entry only adds to them.
type DefaultPrivilege struct {
	Role       string  `json:"role"`
	Schema     string  `json:"schema,omitempty"`
	ObjectType string  `json:"objectType"` // tables | sequences | functions | types | schemas
	Grants     []Grant `json:"grants,omitempty"`
}

// KindDefaultPrivilege identifies a default-privilege entry in references.
const KindDefaultPrivilege ObjKind = "defaultPrivilege"

// Ref identifies the entry: its schema ("" for database-wide), and the role
// and object type it is for.
func (d *DefaultPrivilege) Ref() ObjRef {
	return ObjRef{Kind: KindDefaultPrivilege, Schema: d.Schema, Name: d.Role + "/" + d.ObjectType}
}

// DefaultPrivilegeObjectTypes are the object types ALTER DEFAULT PRIVILEGES
// covers, by their pg_default_acl.defaclobjtype letter.
var DefaultPrivilegeObjectTypes = map[string]string{
	"r": "tables", "S": "sequences", "f": "functions", "T": "types", "n": "schemas",
}

// BuiltinDefaultGrants are the grants PostgreSQL gives every new object of a
// type when no database-wide entry replaces them (the creator's own excluded):
// EXECUTE on routines and USAGE on types to PUBLIC.
func BuiltinDefaultGrants(objectType string) []Grant {
	switch objectType {
	case "functions":
		return []Grant{{Grantee: "", Privileges: []Privilege{{Type: "EXECUTE"}}}}
	case "types":
		return []Grant{{Grantee: "", Privileges: []Privilege{{Type: "USAGE"}}}}
	}
	return nil
}

// DefaultPrivilege looks an entry up by role, schema and object type.
func (d *Database) DefaultPrivilege(role, schema, objectType string) *DefaultPrivilege {
	for _, dp := range d.DefaultPrivileges {
		if dp.Role == role && dp.Schema == schema && dp.ObjectType == objectType {
			return dp
		}
	}
	return nil
}

func sortDefaultPrivileges(dps []*DefaultPrivilege) {
	sort.Slice(dps, func(i, j int) bool {
		a, b := dps[i], dps[j]
		if a.Schema != b.Schema {
			return a.Schema < b.Schema
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.ObjectType < b.ObjectType
	})
	for _, dp := range dps {
		SortGrants(dp.Grants)
	}
}
