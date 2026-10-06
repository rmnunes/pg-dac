package sqlgen

import (
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// renderGrant emits GRANT statements for one grantee on one object: plain
// privileges in one statement and grantable ones in another WITH GRANT OPTION.
// Granting a privilege the grantee already holds is a no-op in PostgreSQL, so
// an upgrade to WITH GRANT OPTION is a plain GRANT ... WITH GRANT OPTION.
func renderGrant(op diff.Op) []Statement {
	var plain, grantable []model.Privilege
	for _, p := range op.Grant.Privileges {
		if p.Grantable {
			grantable = append(grantable, p)
		} else {
			plain = append(plain, p)
		}
	}
	var stmts []Statement
	add := func(privs []model.Privilege, suffix string) {
		if len(privs) == 0 {
			return
		}
		s := Statement{
			SQL:           "GRANT " + privilegeList(privs, op.Sub) + " ON " + grantTarget(op) + " TO " + granteeName(op.Grant.Grantee) + suffix + ";",
			Transactional: true,
		}
		if op.MissingRole {
			s.Hazards = append(s.Hazards, safety.New(safety.MissingRole,
				"role "+model.QuoteIdent(op.Grant.Grantee)+" does not exist in the target; create it (or list it in the project's roles) before publishing"))
		}
		stmts = append(stmts, s)
	}
	add(plain, "")
	add(grantable, " WITH GRANT OPTION")
	return stmts
}

// renderRevoke emits REVOKE statements: privileges marked Grantable lose only
// their grant option, the others are revoked outright.
func renderRevoke(op diff.Op) []Statement {
	var full, option []model.Privilege
	for _, p := range op.Grant.Privileges {
		if p.Grantable {
			option = append(option, p)
		} else {
			full = append(full, p)
		}
	}
	var stmts []Statement
	if len(option) > 0 {
		stmts = append(stmts, Statement{
			SQL:           "REVOKE GRANT OPTION FOR " + privilegeList(option, op.Sub) + " ON " + grantTarget(op) + " FROM " + granteeName(op.Grant.Grantee) + ";",
			Transactional: true,
		})
	}
	if len(full) > 0 {
		stmts = append(stmts, Statement{
			SQL:           "REVOKE " + privilegeList(full, op.Sub) + " ON " + grantTarget(op) + " FROM " + granteeName(op.Grant.Grantee) + ";",
			Transactional: true,
		})
	}
	return stmts
}

// privilegeList renders "SELECT, INSERT" or, for a column, `SELECT ("c"), INSERT ("c")`.
func privilegeList(privs []model.Privilege, column string) string {
	parts := make([]string, len(privs))
	for i, p := range privs {
		parts[i] = p.Type
		if column != "" {
			parts[i] += " (" + model.QuoteIdent(column) + ")"
		}
	}
	return strings.Join(parts, ", ")
}

// grantTarget renders the object clause of GRANT/REVOKE for the op's owner.
func grantTarget(op diff.Op) string {
	o := op.Owner
	switch o.Kind {
	case model.KindSchema:
		return "SCHEMA " + model.QuoteIdent(o.Name)
	case model.KindSequence:
		return "SEQUENCE " + model.QualifiedName(o.Schema, o.Name)
	case model.KindFunction:
		kind := "FUNCTION"
		if op.Function != nil && op.Function.Kind == "procedure" {
			kind = "PROCEDURE"
		}
		// Owner.Name carries the full signature "name(args)".
		name, args := o.Name, ""
		if p := strings.IndexByte(name, '('); p >= 0 {
			name, args = name[:p], name[p:]
		}
		return kind + " " + model.QualifiedName(o.Schema, name) + args
	case model.KindDomain:
		return "DOMAIN " + model.QualifiedName(o.Schema, o.Name)
	case model.KindEnum, model.KindComposite:
		return "TYPE " + model.QualifiedName(o.Schema, o.Name)
	default: // tables, views, materialized views
		return "TABLE " + model.QualifiedName(o.Schema, o.Name)
	}
}

func granteeName(g string) string {
	if g == "" {
		return "PUBLIC"
	}
	return model.QuoteIdent(g)
}
