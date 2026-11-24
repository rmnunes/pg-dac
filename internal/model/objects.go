package model

import "sort"

// Privilege is one privilege a grantee holds on an object (GRANT ... [WITH
// GRANT OPTION]). Type is the keyword PostgreSQL's aclexplode reports: SELECT,
// INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, MAINTAIN, USAGE,
// EXECUTE, CREATE.
type Privilege struct {
	Type      string `json:"type"`
	Grantable bool   `json:"grantable,omitempty"`
}

// Grant is the set of privileges one grantee holds on an object. Grantee "" is
// PUBLIC. An object's grants are its EFFECTIVE ACL (PostgreSQL's built-in
// defaults when nothing was ever granted) minus the owner's own entry — the
// owner holds everything implicitly and ownership is not modeled — and minus
// the grantor, which is always the deploying role. Privileges are sorted by
// Type and an object's grants by Grantee, so equality is positional.
type Grant struct {
	Grantee    string      `json:"grantee"`
	Privileges []Privilege `json:"privileges"`
}

// SortGrants orders grants by grantee and each grant's privileges by type.
func SortGrants(gs []Grant) {
	for i := range gs {
		sort.Slice(gs[i].Privileges, func(a, b int) bool { return gs[i].Privileges[a].Type < gs[i].Privileges[b].Type })
	}
	sort.Slice(gs, func(a, b int) bool { return gs[a].Grantee < gs[b].Grantee })
}

// ViewColumn is one output column of a view (name and canonical type), in
// position order. CREATE OR REPLACE VIEW may only append columns; a rename,
// removal, or type change needs DROP + CREATE (and a recreate of dependents).
type ViewColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// View models a regular view (relkind 'v').
type View struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`
	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`

	// Definition is the canonical pg_get_viewdef output (the SELECT body,
	// without the trailing semicolon). Authoritative equality key.
	Definition string `json:"definition"`

	// CheckOption is "" (none), "local", or "cascaded".
	CheckOption string `json:"checkOption,omitempty"`
	// SecurityBarrier / SecurityInvoker mirror the view options of the same name.
	SecurityBarrier bool `json:"securityBarrier,omitempty"`
	SecurityInvoker bool `json:"securityInvoker,omitempty"`

	// Triggers are the INSTEAD OF / statement triggers defined on the view.
	Triggers []*Trigger `json:"triggers,omitempty"`

	// DependsOn lists the relations/objects this view references, derived from
	// pg_depend/pg_rewrite, so the planner can order view creation after its
	// dependencies and view drops before them.
	DependsOn []ObjRef `json:"dependsOn,omitempty"`

	// Columns is the view's output column list (name + type, in order). It
	// decides whether a changed definition can be applied with CREATE OR REPLACE
	// (only appended columns) or needs DROP + CREATE.
	Columns []ViewColumn `json:"columns,omitempty"`
	// ColumnDeps lists the specific columns this view references (pg_depend
	// refobjsubid > 0). A type change or drop of one of them is blocked by the
	// view, so the engine drops and recreates the view around that change.
	ColumnDeps []ColumnRef `json:"columnDeps,omitempty"`
}

// Ref returns a dependency-graph reference to this view.
func (v *View) Ref() ObjRef { return ObjRef{Kind: KindView, Schema: v.Schema, Name: v.Name} }

// QualifiedName returns the schema-qualified, quoted view name.
func (v *View) QualifiedName() string { return QualifiedName(v.Schema, v.Name) }

// MaterializedView models a materialized view (relkind 'm').
type MaterializedView struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`

	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`

	Definition string      `json:"definition"`
	WithData   bool        `json:"withData,omitempty"`
	DependsOn  []ObjRef    `json:"dependsOn,omitempty"`
	ColumnDeps []ColumnRef `json:"columnDeps,omitempty"`
	Indexes    []*Index    `json:"indexes,omitempty"`
}

// Ref returns a dependency-graph reference to this materialized view.
func (m *MaterializedView) Ref() ObjRef {
	return ObjRef{Kind: KindMatView, Schema: m.Schema, Name: m.Name}
}

// QualifiedName returns the schema-qualified, quoted materialized-view name.
func (m *MaterializedView) QualifiedName() string { return QualifiedName(m.Schema, m.Name) }

// Sequence models a sequence (relkind 'S').
type Sequence struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`

	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`

	DataType  string `json:"dataType,omitempty"` // bigint by default
	Start     int64  `json:"start"`
	Increment int64  `json:"increment"`
	MinValue  int64  `json:"minValue"`
	MaxValue  int64  `json:"maxValue"`
	Cache     int64  `json:"cache"`
	Cycle     bool   `json:"cycle,omitempty"`

	// OwnedBy is set when the sequence is owned by a column (identity/serial).
	// An identity column's sequence is implicit (see Database.IsIdentitySequence)
	// and is never emitted; a serial column's sequence is emitted explicitly —
	// CREATE SEQUENCE before the table, ALTER SEQUENCE ... OWNED BY after it.
	OwnedBy *ColumnRef `json:"ownedBy,omitempty"`
}

// Ref returns a dependency-graph reference to this sequence.
func (s *Sequence) Ref() ObjRef { return ObjRef{Kind: KindSequence, Schema: s.Schema, Name: s.Name} }

// QualifiedName returns the schema-qualified, quoted sequence name.
func (s *Sequence) QualifiedName() string { return QualifiedName(s.Schema, s.Name) }

// IsOwned reports whether the sequence is owned by a column (and thus created
// implicitly with that column rather than as a standalone object).
func (s *Sequence) IsOwned() bool { return s.OwnedBy != nil }

// Function models a function or procedure (pg_proc, prokind 'f' or 'p').
// Functions can be overloaded, so identity is the name plus the argument-type
// signature, not the name alone.
type Function struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// Kind is "function" or "procedure".
	Kind string `json:"kind"`
	// IdentityArgs is the argument-type list (pg_get_function_identity_arguments),
	// e.g. "integer, text". Together with Name it forms the object's identity and
	// the DROP target.
	IdentityArgs string `json:"identityArgs"`
	Owner        string `json:"owner,omitempty"`
	// Definition is the canonical CREATE OR REPLACE statement
	// (pg_get_functiondef). Authoritative equality key — normalized through a real
	// database so body formatting differences are not false diffs.
	Definition string `json:"definition"`
	Language   string `json:"language,omitempty"`
	Comment    string `json:"comment,omitempty"`
	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`

	// ReturnType is pg_get_function_result (e.g. "integer", "SETOF app.t",
	// "TABLE(a integer)"); empty for procedures. CREATE OR REPLACE cannot change
	// it, so a change means DROP + CREATE (and recreating dependents).
	ReturnType string `json:"returnType,omitempty"`
	// Arguments is the full pg_get_function_arguments text (modes, names,
	// defaults). OUT-parameter changes alter the result row type and likewise
	// force DROP + CREATE.
	Arguments string `json:"arguments,omitempty"`

	// DependsOn lists objects that must exist before the function can be
	// created: relation row types and user types (enums) used in its signature,
	// plus relations/functions a SQL-standard (BEGIN ATOMIC) body references.
	// Derived from pg_depend. Plain string bodies record no dependencies — they
	// are covered by applying with check_function_bodies = false.
	DependsOn []ObjRef `json:"dependsOn,omitempty"`
}

// Signature returns "name(argtypes)", the overload-aware identity.
func (f *Function) Signature() string { return f.Name + "(" + f.IdentityArgs + ")" }

// QualifiedSignature returns the schema-qualified, quoted-name signature for use
// in generated SQL (e.g. DROP FUNCTION "app"."f"(integer)).
func (f *Function) QualifiedSignature() string {
	return QualifiedName(f.Schema, f.Name) + "(" + f.IdentityArgs + ")"
}

// Ref returns a dependency-graph reference; Name carries the full signature so
// overloads are distinct.
func (f *Function) Ref() ObjRef {
	return ObjRef{Kind: KindFunction, Schema: f.Schema, Name: f.Signature()}
}

func (f *Function) sortKey() string { return f.Signature() }

// Collation models a user-defined collation (pg_collation). Definition is the
// reconstructed CREATE COLLATION statement (no pg_get_collationdef exists), which
// is the authoritative equality key.
type Collation struct {
	Schema     string `json:"schema"`
	Name       string `json:"name"`
	Owner      string `json:"owner,omitempty"`
	Definition string `json:"definition"`
	Comment    string `json:"comment,omitempty"`
}

// Ref returns a dependency-graph reference to this collation.
func (c *Collation) Ref() ObjRef { return ObjRef{Kind: KindCollation, Schema: c.Schema, Name: c.Name} }

// QualifiedName returns the schema-qualified, quoted collation name.
func (c *Collation) QualifiedName() string { return QualifiedName(c.Schema, c.Name) }

// Domain models a domain type (pg_type typtype 'd'). Definition is the
// reconstructed CREATE DOMAIN statement (base type, collation, default, NOT
// NULL, named CHECK constraints) — the authoritative equality key.
type Domain struct {
	Schema     string `json:"schema"`
	Name       string `json:"name"`
	Owner      string `json:"owner,omitempty"`
	Definition string `json:"definition"`
	Comment    string `json:"comment,omitempty"`
	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`
}

// Ref returns a dependency-graph reference to this domain.
func (d *Domain) Ref() ObjRef { return ObjRef{Kind: KindDomain, Schema: d.Schema, Name: d.Name} }

// QualifiedName returns the schema-qualified, quoted domain name.
func (d *Domain) QualifiedName() string { return QualifiedName(d.Schema, d.Name) }

// CompositeType models a stand-alone composite type (CREATE TYPE ... AS (...)).
// Definition is the reconstructed CREATE TYPE statement.
type CompositeType struct {
	Schema     string `json:"schema"`
	Name       string `json:"name"`
	Owner      string `json:"owner,omitempty"`
	Definition string `json:"definition"`
	Comment    string `json:"comment,omitempty"`
	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`
}

// Ref returns a dependency-graph reference to this composite type.
func (c *CompositeType) Ref() ObjRef {
	return ObjRef{Kind: KindComposite, Schema: c.Schema, Name: c.Name}
}

// QualifiedName returns the schema-qualified, quoted type name.
func (c *CompositeType) QualifiedName() string { return QualifiedName(c.Schema, c.Name) }

// EnumType models an enum type (pg_type + pg_enum).
type EnumType struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`

	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`

	// Values are the enum labels in sort order. Adding a value is online and
	// (PG12+) transaction-safe; removing or reordering values is not supported
	// by PostgreSQL and is reported as a hazard.
	Values []string `json:"values"`
}

// Ref returns a dependency-graph reference to this enum type.
func (e *EnumType) Ref() ObjRef { return ObjRef{Kind: KindEnum, Schema: e.Schema, Name: e.Name} }

// QualifiedName returns the schema-qualified, quoted enum type name.
func (e *EnumType) QualifiedName() string { return QualifiedName(e.Schema, e.Name) }
