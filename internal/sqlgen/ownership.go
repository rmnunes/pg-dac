package sqlgen

import (
	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// renderAlterOwner emits ALTER … OWNER TO for an object the target has under
// another owner. PostgreSQL moves the old owner's ACL entry to the new owner,
// so the previous owner keeps only what is granted to it: a blocking hazard.
func renderAlterOwner(op diff.Op) []Statement {
	s := Statement{SQL: AlterOwnerSQL(op.Owner, op.Function, op.NewOwner), Transactional: true}
	s.Hazards = append(s.Hazards, safety.New(safety.ChangesOwner,
		op.Owner.String()+" is owned by "+model.QuoteIdent(op.PrevOwner)+"; making "+model.QuoteIdent(op.NewOwner)+
			" its owner takes away what "+model.QuoteIdent(op.PrevOwner)+" holds as owner, except privileges granted to it"))
	if op.MissingRole {
		s.Hazards = append(s.Hazards, missingOwner(op.NewOwner))
	}
	return []Statement{s}
}

// AlterOwnerSQL is the ALTER … OWNER TO statement that gives the object ref
// (fn for a routine) to role.
func AlterOwnerSQL(ref model.ObjRef, fn *model.Function, role string) string {
	return "ALTER " + ownedObject(diff.Op{Owner: ref, Function: fn}) + " OWNER TO " + model.QuoteIdent(role) + ";"
}

// ownedObject renders the object clause of ALTER … OWNER TO.
func ownedObject(op diff.Op) string {
	o := op.Owner
	q := model.QualifiedName(o.Schema, o.Name)
	switch o.Kind {
	case model.KindSchema:
		return "SCHEMA " + model.QuoteIdent(o.Name)
	case model.KindView:
		return "VIEW " + q
	case model.KindMatView:
		return "MATERIALIZED VIEW " + q
	case model.KindSequence:
		return "SEQUENCE " + q
	case model.KindFunction:
		return grantTarget(op) // FUNCTION or PROCEDURE with its signature
	case model.KindEnum, model.KindComposite:
		return "TYPE " + q
	case model.KindDomain:
		return "DOMAIN " + q
	case model.KindCollation:
		return "COLLATION " + q
	default:
		return "TABLE " + q
	}
}

// SetRole is the statement that makes what follows run as role, the
// project's owner; ResetRole returns to the role the session logged in as.
// missing marks an owner the target does not have, which blocks a publish.
func SetRole(role string, missing bool) Statement {
	s := Statement{SQL: "SET ROLE " + model.QuoteIdent(role) + ";", Transactional: true}
	if missing {
		s.Hazards = append(s.Hazards, missingOwner(role))
	}
	return s
}

// ResetRole ends a run of statements made as the owner.
func ResetRole() Statement {
	return Statement{SQL: "RESET ROLE;", Transactional: true}
}

func missingOwner(role string) safety.Hazard {
	return safety.New(safety.MissingRole,
		"role "+model.QuoteIdent(role)+", the project's owner, does not exist in the target; create it before publishing")
}
