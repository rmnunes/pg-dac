package model

// View models a regular view (relkind 'v').
type View struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`

	// Definition is the canonical pg_get_viewdef output (the SELECT body,
	// without the trailing semicolon). Authoritative equality key.
	Definition string `json:"definition"`

	// CheckOption is "" (none), "local", or "cascaded".
	CheckOption string `json:"checkOption,omitempty"`

	// DependsOn lists the relations/objects this view references, derived from
	// pg_depend/pg_rewrite, so the planner can order view creation after its
	// dependencies and view drops before them.
	DependsOn []ObjRef `json:"dependsOn,omitempty"`
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

	Definition string   `json:"definition"`
	WithData   bool     `json:"withData,omitempty"`
	DependsOn  []ObjRef `json:"dependsOn,omitempty"`
	Indexes    []*Index `json:"indexes,omitempty"`
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

	DataType  string `json:"dataType,omitempty"` // bigint by default
	Start     int64  `json:"start"`
	Increment int64  `json:"increment"`
	MinValue  int64  `json:"minValue"`
	MaxValue  int64  `json:"maxValue"`
	Cache     int64  `json:"cache"`
	Cycle     bool   `json:"cycle,omitempty"`

	// OwnedBy is set when the sequence is owned by a column (identity/serial),
	// in which case pg-dac does not emit it as a standalone object — it is
	// created implicitly with the owning column.
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
	// Definition is the canonical CREATE OR REPLACE statement
	// (pg_get_functiondef). Authoritative equality key — normalized through a real
	// database so body formatting differences are not false diffs.
	Definition string `json:"definition"`
	Language   string `json:"language,omitempty"`
	Comment    string `json:"comment,omitempty"`
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

// EnumType models an enum type (pg_type + pg_enum).
type EnumType struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`

	// Values are the enum labels in sort order. Adding a value is online and
	// (PG12+) transaction-safe; removing or reordering values is not supported
	// by PostgreSQL and is reported as a hazard.
	Values []string `json:"values"`
}

// Ref returns a dependency-graph reference to this enum type.
func (e *EnumType) Ref() ObjRef { return ObjRef{Kind: KindEnum, Schema: e.Schema, Name: e.Name} }

// QualifiedName returns the schema-qualified, quoted enum type name.
func (e *EnumType) QualifiedName() string { return QualifiedName(e.Schema, e.Name) }
