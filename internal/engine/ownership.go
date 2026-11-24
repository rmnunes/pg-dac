package engine

import (
	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// expandOwnership applies the project's owner (Options.Owner) to the change
// set:
//
//   - the owner's privileges on what the desired state has it own are
//     implicit, so no GRANT or REVOKE for it is emitted there (a REVOKE would
//     take away the owner's own rights once it owns the object);
//   - an object the desired state has under the owner that the target has
//     under another role is reassigned (ALTER … OWNER TO), unless
//     IgnoreOwnership. Only objects the desired state has under the owner are
//     compared: the public schema, which every database comes with, keeps its
//     owner. Objects the migration drops are not reassigned, and what it
//     creates is created as the owner (see withRoles).
func expandOwnership(cs *diff.ChangeSet, from, to *model.Database, opts Options) *diff.ChangeSet {
	if opts.Owner == "" || to == nil {
		return cs
	}
	desired := to.Owners()
	out := &diff.ChangeSet{Warnings: cs.Warnings}
	for _, op := range cs.Ops {
		if (op.Type == diff.OpGrant || op.Type == diff.OpRevoke) && op.Grant != nil &&
			op.Grant.Grantee == opts.Owner && desired[op.Owner.Key()] == opts.Owner {
			continue
		}
		out.Ops = append(out.Ops, op)
	}
	if opts.IgnoreOwnership || from == nil {
		return out
	}

	dropped := map[string]bool{}
	for _, op := range cs.Ops {
		if op.Type.IsObjectDrop() {
			dropped[op.Owner.Key()] = true
		}
	}
	current := map[string]model.OwnedObject{}
	for _, o := range from.OwnedObjects() {
		current[o.Ref.Key()] = o
	}
	missing := ownerMissing(from, opts.Owner)
	var added []diff.Op
	for _, o := range to.OwnedObjects() {
		if o.Linked || o.Owner != opts.Owner {
			continue
		}
		key := o.Ref.Key()
		t, ok := current[key]
		if !ok || t.Linked || t.Owner == "" || t.Owner == opts.Owner || dropped[key] {
			continue
		}
		added = append(added, diff.Op{Type: diff.OpAlterOwner, Owner: o.Ref, Function: o.Function,
			PrevOwner: t.Owner, NewOwner: opts.Owner, MissingRole: missing})
	}
	out.Ops = append(out.Ops, filterByObject(&diff.ChangeSet{Ops: added}, opts).Ops...)
	return out
}

// ownerMissing reports whether a live target lacks the owner role.
func ownerMissing(from *model.Database, owner string) bool {
	if from == nil || len(from.Roles) == 0 || owner == "" {
		return false
	}
	for _, r := range from.Roles {
		if r == owner {
			return false
		}
	}
	return true
}

// acting says which role an op's statements run as.
type acting int

const (
	actKeep    acting = iota // either: stay as whatever the previous statement ran as
	actOwner                 // the project's owner (SET ROLE)
	actSession               // the role the session logged in as (RESET ROLE)
)

// withRoles flattens the per-op statements into the migration, switching
// roles between them when there is a project owner: each object is changed
// as its owner — what the migration creates and what the owner owns as the
// owner, what another role owns as the deploying role (which must be able to
// act for that role, as it must today) — so PostgreSQL's ownership checks
// pass and new objects are owned by, and get the default privileges of, the
// owner. Extensions and ALTER … OWNER TO run as the deploying role: one
// needs its rights, the other membership in the previous owner. It reports
// whether any statement runs as the owner.
func withRoles(groups []sqlgen.OpStatements, from *model.Database, opts Options) ([]sqlgen.Statement, bool, []CreateRight) {
	var flat []sqlgen.Statement
	if opts.Owner == "" {
		for _, g := range groups {
			flat = append(flat, g.Statements...)
		}
		return flat, false, nil
	}
	owners := from.Owners()
	missing := ownerMissing(from, opts.Owner)
	asOwner, used := false, false
	var rights []CreateRight
	seen := map[CreateRight]bool{}
	need := func(r CreateRight) {
		if !seen[r] {
			seen[r] = true
			rights = append(rights, r)
		}
	}
	schemaKey := func(name string) string { return model.ObjRef{Kind: model.KindSchema, Name: name}.Key() }
	for _, g := range groups {
		op := g.Op
		// What PostgreSQL checks: CREATE on the database for whoever creates
		// a schema as the owner or gives one to it (ALTER SCHEMA … OWNER TO
		// checks the role running it); CREATE on the schema for the owner
		// when it creates or takes an object in a schema it does not own.
		inForeignSchema := op.Owner.Schema != "" && owners[schemaKey(op.Owner.Schema)] != opts.Owner
		_, existed := owners[op.Owner.Key()]
		role := actingRole(op, owners, opts.Owner)
		switch {
		case op.Type == diff.OpAlterOwner && op.Owner.Kind == model.KindSchema:
			need(CreateRight{})
		case op.Type == diff.OpAlterOwner && inForeignSchema:
			need(CreateRight{Role: opts.Owner, Schema: op.Owner.Schema})
		case op.Type == diff.OpCreateSchema && !existed:
			need(CreateRight{Role: opts.Owner})
		case op.Type.IsObjectCreate() && !existed && role == actOwner && inForeignSchema:
			need(CreateRight{Role: opts.Owner, Schema: op.Owner.Schema})
		}
		if len(g.Statements) == 0 {
			continue
		}
		switch {
		case role == actOwner && !asOwner:
			flat = append(flat, sqlgen.SetRole(opts.Owner, missing && !used))
			asOwner, used = true, true
		case role == actSession && asOwner:
			flat = append(flat, sqlgen.ResetRole())
			asOwner = false
		}
		flat = append(flat, g.Statements...)
	}
	if asOwner {
		flat = append(flat, sqlgen.ResetRole())
	}
	return flat, used, rights
}

// CreateRight is a CREATE privilege a migration with a project owner needs on
// the target: on the database when Schema is "", else on that schema; held by
// Role, or by the deploying role when Role is "".
type CreateRight struct {
	Role, Schema string
}

// actingRole decides the role for one op and records what it does to
// ownership in owners (object key -> owner as the migration proceeds).
func actingRole(op diff.Op, owners map[string]string, owner string) acting {
	switch op.Type {
	case diff.OpCreateExtension, diff.OpDropExtension, diff.OpAlterExtension:
		return actSession
	case diff.OpAlterOwner:
		owners[op.Owner.Key()] = op.NewOwner
		return actSession
	case diff.OpGrantDefault, diff.OpRevokeDefault:
		if op.DefaultPriv != nil && op.DefaultPriv.Role == owner {
			return actOwner
		}
		return actSession
	case diff.OpUnsupported:
		return actKeep
	}
	if !ownable(op.Owner.Kind) {
		return actSession
	}
	key := op.Owner.Key()
	cur, exists := owners[key]
	switch {
	case op.Type.IsObjectCreate() && !exists:
		// A sequence a column will own is created as the owner of that
		// column's table: PostgreSQL requires the two to match.
		cur = owner
		if op.Type == diff.OpCreateSequence && op.Sequence != nil && op.Sequence.OwnedBy != nil {
			tbl := model.ObjRef{Kind: model.KindTable, Schema: op.Sequence.OwnedBy.Schema, Name: op.Sequence.OwnedBy.Table}
			if o, ok := owners[tbl.Key()]; ok {
				cur = o
			}
		}
		owners[key] = cur
	case op.Type.IsObjectDrop():
		delete(owners, key)
	case !exists:
		// Part of an object this migration creates.
		return actOwner
	}
	if cur == owner {
		return actOwner
	}
	return actSession
}

func ownable(k model.ObjKind) bool {
	switch k {
	case model.KindSchema, model.KindTable, model.KindView, model.KindMatView, model.KindSequence,
		model.KindFunction, model.KindEnum, model.KindDomain, model.KindComposite, model.KindCollation:
		return true
	}
	return false
}
