// Package engine ties the comparison pipeline together: it diffs two models,
// applies deploy options (drop policy), orders the result into a plan, and
// renders lock-safe SQL. It is the single entry point the CLI verbs call,
// regardless of whether each side is a live database, a .pgdac artifact, or a
// declarative project — those are resolved to models before reaching here.
package engine

import (
	"fmt"
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/plan"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// Options controls comparison and generation. The defaults mirror the
// conservative DACPAC/SqlPackage posture: do not drop objects missing from the
// source, and block data loss unless explicitly allowed.
type Options struct {
	// TargetMajor overrides the PostgreSQL major version used for version-gated
	// strategies. 0 means "derive from the target/desired model".
	TargetMajor int
	// Online enables lock-safe rewrites (CONCURRENTLY, NOT VALID/VALIDATE, ...).
	Online bool

	// AllowHazards permits data-loss operations to be applied (the
	// BlockOnPossibleDataLoss analog). It does not affect script/report output,
	// only whether a publish is blocked.
	AllowHazards bool

	// DropObjectsNotInSource drops objects (schemas, tables, columns, views,
	// matviews, sequences, enums, extensions) that exist in the target but not in
	// the source. Default false.
	DropObjectsNotInSource bool
	// DropConstraintsNotInSource / DropIndexesNotInSource drop constraints/indexes
	// missing from the source. Default true (they are cheap to recreate).
	DropConstraintsNotInSource bool
	DropIndexesNotInSource     bool

	// CompareWhitespace / IgnoreComments tune how routine bodies are compared
	// (see diff.Options). Defaults: whitespace is ignored, comments count.
	CompareWhitespace bool
	IgnoreComments    bool
	// IgnorePrivileges leaves GRANT/REVOKE out of the comparison (SSDT's
	// IgnorePermissions). Objects pg-dac recreates still get their privileges
	// back. Revokes of privileges the source does not hold follow
	// DropObjectsNotInSource (DropPermissionsNotInSource).
	IgnorePrivileges bool
	// ColumnOrder is the policy for a declared column order the target does not
	// have: "" / "warn" reports it, "ignore" is silent, "rebuild" rebuilds the
	// table in the declared order (see diff.Options).
	ColumnOrder string

	// ExcludeObjectTypes leaves whole kinds of object out of the comparison
	// (SSDT's ExcludeObjectTypes): table, view, matview, sequence, function,
	// enum, domain, compositeType, collation, schema, extension, index,
	// constraint, trigger, policy, privilege, comment. An excluded kind is
	// neither created, altered nor dropped; everything else proceeds.
	ExcludeObjectTypes []string

	// IncludeIDs / ExcludeIDs restrict the comparison to a chosen set of objects,
	// keyed by object ID (model.ObjRef.Key() — what ObjectChange.ID reports). When
	// IncludeIDs is non-empty, only those objects are changed; ExcludeIDs always
	// removes objects. Filtering happens BEFORE planning so dependency ordering is
	// recomputed over the surviving set — this is how the GUI's per-object
	// include/exclude stays dependency-correct and plan-vs-apply consistent.
	IncludeIDs map[string]bool
	ExcludeIDs map[string]bool
}

// DefaultOptions returns the recommended conservative defaults.
func DefaultOptions() Options {
	return Options{
		Online:                     true,
		DropConstraintsNotInSource: true,
		DropIndexesNotInSource:     true,
	}
}

// Result holds the products of a comparison.
type Result struct {
	ChangeSet  *diff.ChangeSet
	Plan       *plan.Plan
	Groups     []sqlgen.OpStatements // per-op statements, preserving the op<->SQL link
	Statements []sqlgen.Statement    // flattened, in plan order (script/apply consume this)
	// Warnings are problems with the generated migration itself (e.g. a
	// dependency cycle the planner could not order); callers show them to the
	// user alongside the script.
	Warnings []string
	// Prelude holds statements that run before the migration and are not part
	// of the diff: refactorlog renames and their bookkeeping. Script() emits
	// them first; publish executes them before re-reading the target.
	Prelude []sqlgen.Statement

	from, to *model.Database // retained for per-object before/after definition rendering
	online   bool
}

// Empty reports whether there is nothing to do.
func (r *Result) Empty() bool { return len(r.Statements) == 0 }

// Compare computes the migration to transform the "from" model (current/target)
// into the "to" model (desired).
func Compare(from, to *model.Database, opts Options) (*Result, error) {
	cs := diff.DiffWith(from, to, opts.diffOptions())
	diffWarnings := cs.Warnings
	cs = filterByType(cs, opts)
	cs = applyRebuildPolicy(cs, opts)
	cs = filterDrops(cs, opts)
	cs = filterByObject(cs, opts)
	cs = expandDependents(cs, from, to)
	cs = expandPrivileges(cs, from, to, opts)
	cs = filterByType(cs, opts) // privileges and dependents added above obey the exclusion too

	p := plan.Build(cs)

	major := opts.TargetMajor
	if major == 0 {
		major = to.MajorVersion()
	}
	if major == 0 {
		major = from.MajorVersion()
	}

	groups, err := sqlgen.GenerateGrouped(p, sqlgen.Options{TargetMajor: major, Online: opts.Online})
	if err != nil {
		return nil, err
	}
	var stmts []sqlgen.Statement
	for _, g := range groups {
		stmts = append(stmts, g.Statements...)
	}
	res := &Result{ChangeSet: cs, Plan: p, Groups: groups, Statements: stmts, from: from, to: to, online: opts.Online}
	res.Warnings = append(res.Warnings, diffWarnings...)
	if len(p.Cyclic) > 0 {
		names := make([]string, 0, len(p.Cyclic))
		for _, op := range p.Cyclic {
			names = append(names, object(op))
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"dependency cycle: %d operation(s) could not be ordered and were appended in priority order; the migration may fail to apply — check: %s",
			len(p.Cyclic), strings.Join(names, "; ")))
	}
	return res, nil
}

// diffOptions maps the comparison options onto the differ's.
func (o Options) diffOptions() diff.Options {
	return diff.Options{CompareWhitespace: o.CompareWhitespace, IgnoreComments: o.IgnoreComments, ColumnOrder: o.ColumnOrder}
}

// filterByType drops every op of an excluded object type (see
// Options.ExcludeObjectTypes). Sub-object ops are classified by the sub-object
// (an index, a constraint, a trigger, a policy, a privilege, a comment), other
// ops by the kind of the object they act on.
func filterByType(cs *diff.ChangeSet, opts Options) *diff.ChangeSet {
	if len(opts.ExcludeObjectTypes) == 0 {
		return cs
	}
	excluded := map[string]bool{}
	for _, t := range opts.ExcludeObjectTypes {
		excluded[t] = true
	}
	out := &diff.ChangeSet{Warnings: cs.Warnings}
	for _, op := range cs.Ops {
		// An excluded kind takes its sub-objects, comments and privileges with
		// it (excluding "view" excludes a view's comment too), and an excluded
		// sub-object kind is excluded on every owner.
		if !excluded[objectTypeOf(op)] && !excluded[string(op.Owner.Kind)] {
			out.Ops = append(out.Ops, op)
		}
	}
	return out
}

// objectTypeOf names the object type an op changes, in the vocabulary of
// Options.ExcludeObjectTypes.
func objectTypeOf(op diff.Op) string {
	switch op.Type {
	case diff.OpCreateIndex, diff.OpDropIndex, diff.OpReplaceIndex:
		return "index"
	case diff.OpAddConstraint, diff.OpDropConstraint, diff.OpValidateConstraint:
		return "constraint"
	case diff.OpCreateTrigger, diff.OpDropTrigger, diff.OpSetTriggerEnabled:
		return "trigger"
	case diff.OpCreatePolicy, diff.OpDropPolicy, diff.OpEnableRLS, diff.OpDisableRLS:
		return "policy"
	case diff.OpGrant, diff.OpRevoke:
		return "privilege"
	case diff.OpSetComment:
		return "comment"
	}
	return string(op.Owner.Kind)
}

// filterByObject keeps only ops whose owning object passes the include/exclude
// sets. Filtering by object (op.Owner.Key()) keeps drop+recreate pairs and a
// table's sub-object ops together, so the subsequent re-plan stays correct.
func filterByObject(cs *diff.ChangeSet, opts Options) *diff.ChangeSet {
	if len(opts.IncludeIDs) == 0 && len(opts.ExcludeIDs) == 0 {
		return cs
	}
	out := &diff.ChangeSet{}
	for _, op := range cs.Ops {
		key := op.Owner.Key()
		if opts.ExcludeIDs[key] {
			continue
		}
		if len(opts.IncludeIDs) > 0 && !opts.IncludeIDs[key] {
			continue
		}
		out.Ops = append(out.Ops, op)
	}
	return out
}

// filterDrops removes drop operations disallowed by the deploy options. A drop
// that is paired with a recreate of the same object (a "changed" object, not a
// removed one) is always kept — otherwise the recreate would fail or the change
// would be silently lost.
func filterDrops(cs *diff.ChangeSet, opts Options) *diff.ChangeSet {
	created := createdIdentities(cs)
	out := &diff.ChangeSet{}
	for _, op := range cs.Ops {
		if !op.Type.IsDrop() {
			out.Ops = append(out.Ops, op)
			continue
		}
		if _, isReplace := created[identity(op)]; isReplace {
			out.Ops = append(out.Ops, op)
			continue
		}
		if dropAllowed(op, opts) {
			out.Ops = append(out.Ops, op)
		}
	}
	return out
}

// dropAllowed applies the drop policy to one drop op. What pg-dac itself left
// behind (Op.Cleanup) is never kept.
func dropAllowed(op diff.Op, opts Options) bool {
	if op.Cleanup {
		return true
	}
	switch op.Type {
	case diff.OpDropConstraint:
		return opts.DropConstraintsNotInSource
	case diff.OpDropIndex:
		return opts.DropIndexesNotInSource
	default:
		return opts.DropObjectsNotInSource
	}
}

// identity returns a key that uniquely names the object an op acts on, so a drop
// and its paired recreate map to the same key.
func identity(op diff.Op) string {
	switch op.Type {
	case diff.OpAddConstraint, diff.OpDropConstraint:
		return op.Owner.Key() + "#c#" + op.Sub
	case diff.OpCreateIndex, diff.OpDropIndex, diff.OpReplaceIndex:
		return op.Owner.Key() + "#i#" + op.Sub
	case diff.OpAddColumn, diff.OpDropColumn:
		return op.Owner.Key() + "#col#" + op.Sub
	case diff.OpCreateTrigger, diff.OpDropTrigger:
		return op.Owner.Key() + "#tg#" + op.Sub
	case diff.OpCreatePolicy, diff.OpDropPolicy:
		return op.Owner.Key() + "#pol#" + op.Sub
	default:
		// Functions key on Owner (whose Name is the full signature); tables,
		// views, etc. key on Owner directly.
		return op.Owner.Key()
	}
}

func createdIdentities(cs *diff.ChangeSet) map[string]struct{} {
	m := map[string]struct{}{}
	for _, op := range cs.Ops {
		switch op.Type {
		case diff.OpCreateTable, diff.OpCreateView, diff.OpCreateMatView,
			diff.OpCreateEnum, diff.OpCreateCollation, diff.OpCreateSequence, diff.OpCreateSchema, diff.OpCreateExtension,
			diff.OpAddConstraint, diff.OpCreateIndex, diff.OpReplaceIndex, diff.OpAddColumn,
			diff.OpCreateFunction, diff.OpCreateTrigger, diff.OpCreatePolicy:
			m[identity(op)] = struct{}{}
		}
	}
	return m
}
