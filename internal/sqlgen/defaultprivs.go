package sqlgen

import (
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// defaultPrivHead renders "ALTER DEFAULT PRIVILEGES FOR ROLE r [IN SCHEMA s]".
func defaultPrivHead(dp *model.DefaultPrivilege) string {
	head := "ALTER DEFAULT PRIVILEGES FOR ROLE " + model.QuoteIdent(dp.Role)
	if dp.Schema != "" {
		head += " IN SCHEMA " + model.QuoteIdent(dp.Schema)
	}
	return head
}

// renderGrantDefault grants a default privilege, like renderGrant: plain
// privileges in one statement, grantable ones WITH GRANT OPTION in another.
// It needs the deploying role to be a member of the entry's role (or a
// superuser); PostgreSQL refuses otherwise.
func renderGrantDefault(op diff.Op) []Statement {
	dp := op.DefaultPriv
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
			SQL: defaultPrivHead(dp) + " GRANT " + privilegeList(privs, "") + " ON " + strings.ToUpper(dp.ObjectType) +
				" TO " + granteeName(op.Grant.Grantee) + suffix + ";",
			Transactional: true,
		}
		if op.MissingForRole {
			s.Hazards = append(s.Hazards, safety.New(safety.MissingRole,
				"role "+model.QuoteIdent(dp.Role)+" does not exist in the target; create it (or list it in the project's roles) before publishing"))
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

// renderRevokeDefault revokes a default privilege; a privilege marked
// Grantable loses only its grant option.
func renderRevokeDefault(op diff.Op) []Statement {
	dp := op.DefaultPriv
	var full, option []model.Privilege
	for _, p := range op.Grant.Privileges {
		if p.Grantable {
			option = append(option, p)
		} else {
			full = append(full, p)
		}
	}
	target := " ON " + strings.ToUpper(dp.ObjectType) + " FROM " + granteeName(op.Grant.Grantee) + ";"
	var stmts []Statement
	if len(option) > 0 {
		stmts = append(stmts, Statement{SQL: defaultPrivHead(dp) + " REVOKE GRANT OPTION FOR " + privilegeList(option, "") + target, Transactional: true})
	}
	if len(full) > 0 {
		stmts = append(stmts, Statement{SQL: defaultPrivHead(dp) + " REVOKE " + privilegeList(full, "") + target, Transactional: true})
	}
	return stmts
}
