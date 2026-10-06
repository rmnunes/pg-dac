package engine

import (
	"sort"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// expandPrivileges adds the GRANT / REVOKE operations that bring every object's
// privileges to the desired state. It runs after the dependents pass because
// what it emits depends on which objects the migration (re)creates:
//
//   - an object that is created or recreated has NO privileges afterwards, so
//     every desired privilege is granted; when the drop policy would not have
//     revoked what the target held, those privileges are granted back too — a
//     recreate pg-dac decided on must not quietly strip access;
//   - an object that persists gets GRANTs for what the source adds (including
//     grant-option upgrades) and REVOKEs for what the source no longer holds,
//     the latter only under the drop policy that drops objects not in the
//     source (SSDT's DropPermissionsNotInSource);
//   - with IgnorePrivileges, nothing is compared, but a recreated object still
//     gets the target's own privileges back, so the option never changes what a
//     role can do.
//
// Grants to a role the live target lacks are marked so sqlgen attaches a
// blocking hazard: PostgreSQL would fail the GRANT half-way through a publish.
func expandPrivileges(cs *diff.ChangeSet, from, to *model.Database, opts Options) *diff.ChangeSet {
	if to == nil {
		return cs
	}
	created := map[string]bool{}
	addedCols := map[string]bool{}
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpCreateTable, diff.OpCreateView, diff.OpCreateMatView, diff.OpCreateSequence,
			diff.OpCreateFunction, diff.OpCreateSchema, diff.OpCreateEnum, diff.OpCreateDomain, diff.OpCreateCompositeType:
			created[op.Owner.Key()] = true
		case diff.OpAddColumn:
			addedCols[colKey(op.Owner, op.Sub)] = true
		}
	}
	var roles map[string]bool
	if from != nil && len(from.Roles) > 0 {
		roles = map[string]bool{}
		for _, r := range from.Roles {
			roles[r] = true
		}
	}

	var added []diff.Op
	// emit computes the privilege ops for one object (or column, when sub is set).
	emit := func(owner model.ObjRef, sub string, fn *model.Function, fromGrants, toGrants []model.Grant, exists, recreated bool) {
		current := acl{}
		if exists && !recreated {
			current = aclOf(fromGrants)
		}
		desired := acl{}
		switch {
		case opts.IgnorePrivileges:
			if recreated && exists {
				desired = aclOf(fromGrants)
			}
		default:
			desired = aclOf(toGrants)
			if recreated && exists && !opts.DropObjectsNotInSource {
				desired.union(aclOf(fromGrants))
			}
		}
		for _, g := range grantsNeeded(current, desired) {
			g := g
			added = append(added, diff.Op{Type: diff.OpGrant, Owner: owner, Sub: sub, Function: fn, Grant: &g,
				MissingRole: roles != nil && g.Grantee != "" && !roles[g.Grantee]})
		}
		if !opts.IgnorePrivileges && exists && !recreated && opts.DropObjectsNotInSource {
			for _, g := range revokesNeeded(current, desired) {
				g := g
				added = append(added, diff.Op{Type: diff.OpRevoke, Owner: owner, Sub: sub, Function: fn, Grant: &g})
			}
		}
	}

	fromSchemas := map[string]*model.Schema{}
	if from != nil {
		for _, s := range from.Schemas {
			fromSchemas[s.Name] = s
		}
	}
	for _, s := range to.Schemas {
		fs := fromSchemas[s.Name]
		sref := model.ObjRef{Kind: model.KindSchema, Name: s.Name}
		var fsGrants []model.Grant
		if fs != nil {
			fsGrants = fs.Grants
		}
		emit(sref, "", nil, fsGrants, s.Grants, fs != nil, created[sref.Key()])

		for _, t := range s.Tables {
			var ft *model.Table
			if fs != nil {
				ft = tableNamed(fs, t.Name)
			}
			var ftGrants []model.Grant
			if ft != nil {
				ftGrants = ft.Grants
			}
			key := t.Ref().Key()
			emit(t.Ref(), "", nil, ftGrants, t.Grants, ft != nil, created[key])
			for _, c := range t.Columns {
				var fc *model.Column
				if ft != nil {
					fc = ft.Column(c.Name)
				}
				var fcGrants []model.Grant
				if fc != nil {
					fcGrants = fc.Grants
				}
				emit(t.Ref(), c.Name, nil, fcGrants, c.Grants, fc != nil, created[key] || addedCols[colKey(t.Ref(), c.Name)])
			}
		}
		for _, v := range s.Views {
			var fg []model.Grant
			exists := false
			if fs != nil {
				for _, fv := range fs.Views {
					if fv.Name == v.Name {
						fg, exists = fv.Grants, true
					}
				}
			}
			emit(v.Ref(), "", nil, fg, v.Grants, exists, created[v.Ref().Key()])
		}
		for _, m := range s.MatViews {
			var fg []model.Grant
			exists := false
			if fs != nil {
				for _, fm := range fs.MatViews {
					if fm.Name == m.Name {
						fg, exists = fm.Grants, true
					}
				}
			}
			emit(m.Ref(), "", nil, fg, m.Grants, exists, created[m.Ref().Key()])
		}
		for _, q := range s.Sequences {
			if to.IsIdentitySequence(q) {
				continue // created implicitly; privileges on identity sequences are not modeled separately
			}
			var fg []model.Grant
			exists := false
			if fs != nil {
				for _, fq := range fs.Sequences {
					if fq.Name == q.Name {
						fg, exists = fq.Grants, true
					}
				}
			}
			emit(q.Ref(), "", nil, fg, q.Grants, exists, created[q.Ref().Key()])
		}
		for _, f := range s.Functions {
			var fg []model.Grant
			exists := false
			if fs != nil {
				for _, ff := range fs.Functions {
					if ff.Signature() == f.Signature() {
						fg, exists = ff.Grants, true
					}
				}
			}
			emit(f.Ref(), "", f, fg, f.Grants, exists, created[f.Ref().Key()])
		}
		for _, e := range s.Enums {
			var fg []model.Grant
			exists := false
			if fs != nil {
				for _, fe := range fs.Enums {
					if fe.Name == e.Name {
						fg, exists = fe.Grants, true
					}
				}
			}
			emit(e.Ref(), "", nil, fg, e.Grants, exists, created[e.Ref().Key()])
		}
		for _, d := range s.Domains {
			var fg []model.Grant
			exists := false
			if fs != nil {
				for _, fd := range fs.Domains {
					if fd.Name == d.Name {
						fg, exists = fd.Grants, true
					}
				}
			}
			emit(d.Ref(), "", nil, fg, d.Grants, exists, created[d.Ref().Key()])
		}
		for _, c := range s.Composites {
			var fg []model.Grant
			exists := false
			if fs != nil {
				for _, fc := range fs.Composites {
					if fc.Name == c.Name {
						fg, exists = fc.Grants, true
					}
				}
			}
			emit(c.Ref(), "", nil, fg, c.Grants, exists, created[c.Ref().Key()])
		}
	}
	if len(added) == 0 {
		return cs
	}
	// Per-object include/exclude applies to privilege ops like to any other.
	filtered := filterByObject(&diff.ChangeSet{Ops: added}, opts)
	out := &diff.ChangeSet{Ops: append(append([]diff.Op{}, cs.Ops...), filtered.Ops...), Warnings: cs.Warnings}
	return out
}

func tableNamed(s *model.Schema, name string) *model.Table {
	for _, t := range s.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// acl is a grantee -> privilege type -> grantable map.
type acl map[string]map[string]bool

func aclOf(gs []model.Grant) acl {
	a := acl{}
	for _, g := range gs {
		m, ok := a[g.Grantee]
		if !ok {
			m = map[string]bool{}
			a[g.Grantee] = m
		}
		for _, p := range g.Privileges {
			m[p.Type] = m[p.Type] || p.Grantable
		}
	}
	return a
}

// union adds every privilege of b (keeping the stronger grant option).
func (a acl) union(b acl) {
	for grantee, privs := range b {
		m, ok := a[grantee]
		if !ok {
			m = map[string]bool{}
			a[grantee] = m
		}
		for p, grantable := range privs {
			m[p] = m[p] || grantable
		}
	}
}

// grantsNeeded lists, per grantee, the privileges desired holds that current
// lacks, plus the ones whose grant option must be added (Grantable carries the
// state to reach).
func grantsNeeded(current, desired acl) []model.Grant {
	var out []model.Grant
	for _, grantee := range sortedGrantees(desired) {
		var privs []model.Privilege
		for _, p := range sortedPrivs(desired[grantee]) {
			want := desired[grantee][p]
			have, ok := current[grantee][p]
			if !ok || (want && !have) {
				privs = append(privs, model.Privilege{Type: p, Grantable: want})
			}
		}
		if len(privs) > 0 {
			out = append(out, model.Grant{Grantee: grantee, Privileges: privs})
		}
	}
	return out
}

// revokesNeeded lists, per grantee, the privileges current holds that desired
// lacks (Grantable=false: revoke it) and those that must lose only their grant
// option (Grantable=true).
func revokesNeeded(current, desired acl) []model.Grant {
	var out []model.Grant
	for _, grantee := range sortedGrantees(current) {
		var privs []model.Privilege
		for _, p := range sortedPrivs(current[grantee]) {
			have := current[grantee][p]
			want, ok := desired[grantee][p]
			switch {
			case !ok:
				privs = append(privs, model.Privilege{Type: p})
			case have && !want:
				privs = append(privs, model.Privilege{Type: p, Grantable: true})
			}
		}
		if len(privs) > 0 {
			out = append(out, model.Grant{Grantee: grantee, Privileges: privs})
		}
	}
	return out
}

func sortedGrantees(a acl) []string {
	out := make([]string, 0, len(a))
	for g := range a {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func sortedPrivs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
