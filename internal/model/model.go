// Package model defines pg-dac's canonical, serializable representation of a
// PostgreSQL database schema. Both sides of every comparison (a live database,
// a .pgdac artifact, or a declarative .sql project) are reduced to this model
// before diffing, so the diff engine never compares raw SQL text.
//
// Design rules (see the project plan):
//   - Objects are keyed on fully-qualified, search_path-normalized NAMES, never
//     on OIDs (OIDs are not stable across dump/restore).
//   - Complex objects (constraints, indexes, views, functions) additionally carry
//     the canonical definition string produced by PostgreSQL's pg_get_*def()
//     functions. Because both sides of a diff are introspected through the same
//     path ("normalize via a real database"), canonical-definition string equality
//     is a sound equality test, while the structured fields drive dependency
//     ordering and targeted ALTERs.
package model

import (
	"fmt"
	"sort"
)

// Database is the top-level model of a single PostgreSQL database's schema.
// It is the unit that gets serialized into a .pgdac artifact's model.json.
type Database struct {
	// TargetVersion is the PostgreSQL server_version_num the model was built
	// against (e.g. 160004 for 16.4). Zero means "unspecified". Version-gated
	// migration strategies key off the major version derived from this.
	TargetVersion int `json:"targetVersion,omitempty"`

	// Extensions installed in the database (the CREATE EXTENSION unit; their
	// member objects are filtered out during introspection).
	Extensions []*Extension `json:"extensions,omitempty"`

	// Schemas (namespaces) and everything they contain.
	Schemas []*Schema `json:"schemas"`
}

// MajorVersion returns the PostgreSQL major version (e.g. 16) derived from
// TargetVersion, or 0 if unknown. Since PostgreSQL 10 the major version is
// server_version_num / 10000.
func (d *Database) MajorVersion() int {
	if d.TargetVersion <= 0 {
		return 0
	}
	return d.TargetVersion / 10000
}

// Table looks up a table by schema and name, returning nil if absent.
func (d *Database) Table(schema, name string) *Table {
	for _, s := range d.Schemas {
		if s.Name != schema {
			continue
		}
		for _, t := range s.Tables {
			if t.Name == name {
				return t
			}
		}
	}
	return nil
}

// HasColumn reports whether the referenced column exists in the model.
func (d *Database) HasColumn(ref ColumnRef) bool {
	t := d.Table(ref.Schema, ref.Table)
	return t != nil && t.Column(ref.Column) != nil
}

// IsIdentitySequence reports whether seq is the implicit sequence behind a
// GENERATED ... AS IDENTITY column. PostgreSQL creates and drops such a
// sequence together with its column, so it must never be emitted standalone.
// A serial-style sequence (owned by a column whose DEFAULT calls nextval) is
// NOT implicit: it has to be created before its table and attached with
// OWNED BY afterwards, or the table's default references a missing relation.
func (d *Database) IsIdentitySequence(seq *Sequence) bool {
	if seq == nil || seq.OwnedBy == nil {
		return false
	}
	t := d.Table(seq.OwnedBy.Schema, seq.OwnedBy.Table)
	if t == nil {
		return false
	}
	c := t.Column(seq.OwnedBy.Column)
	return c != nil && c.Identity != ""
}

// Extension models an installed PostgreSQL extension.
type Extension struct {
	Name    string `json:"name"`
	Schema  string `json:"schema,omitempty"`
	Version string `json:"version,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// Schema models a PostgreSQL namespace and the objects it owns.
type Schema struct {
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`

	Tables     []*Table            `json:"tables,omitempty"`
	Views      []*View             `json:"views,omitempty"`
	MatViews   []*MaterializedView `json:"matViews,omitempty"`
	Sequences  []*Sequence         `json:"sequences,omitempty"`
	Enums      []*EnumType         `json:"enums,omitempty"`
	Domains    []*Domain           `json:"domains,omitempty"`
	Composites []*CompositeType    `json:"compositeTypes,omitempty"`
	Collations []*Collation        `json:"collations,omitempty"`
	Functions  []*Function         `json:"functions,omitempty"`
}

// ObjRef is a fully-qualified reference to a schema object, used to model
// inter-object dependencies (e.g. a view's referenced tables) without OIDs.
type ObjRef struct {
	Kind   ObjKind `json:"kind"`
	Schema string  `json:"schema"`
	Name   string  `json:"name"`
}

// ColumnRef references a specific column of a relation.
type ColumnRef struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Column string `json:"column"`
}

// ObjKind enumerates the kinds of schema objects pg-dac models. Its values are
// also used as the node "kind" in the dependency graph and as a coarse ordering
// key (see internal/plan).
type ObjKind string

const (
	KindExtension  ObjKind = "extension"
	KindSchema     ObjKind = "schema"
	KindEnum       ObjKind = "enum"
	KindDomain     ObjKind = "domain"
	KindComposite  ObjKind = "compositeType"
	KindCollation  ObjKind = "collation"
	KindSequence   ObjKind = "sequence"
	KindFunction   ObjKind = "function"
	KindTable      ObjKind = "table"
	KindColumn     ObjKind = "column"
	KindConstraint ObjKind = "constraint"
	KindIndex      ObjKind = "index"
	KindTrigger    ObjKind = "trigger"
	KindPolicy     ObjKind = "policy"
	KindView       ObjKind = "view"
	KindMatView    ObjKind = "matview"
)

// QualifiedName returns the schema-qualified, identifier-quoted name for an
// object, suitable for use in generated SQL and as a stable map key.
func QualifiedName(schema, name string) string {
	return QuoteIdent(schema) + "." + QuoteIdent(name)
}

// Key returns the canonical lookup key for an object reference.
func (r ObjRef) Key() string { return string(r.Kind) + ":" + QualifiedName(r.Schema, r.Name) }

// String renders an ObjRef for human-readable output. Objects that have no
// parent schema (schemas and extensions) are rendered by name alone.
func (r ObjRef) String() string {
	name := QuoteIdent(r.Name)
	if r.Schema != "" {
		name = QualifiedName(r.Schema, r.Name)
	}
	return fmt.Sprintf("%s %s", r.Kind, name)
}

// Sort orders all collections in the database model by name, recursively, so
// that serialization and diffing are deterministic. It mutates the receiver.
func (d *Database) Sort() {
	sort.Slice(d.Extensions, func(i, j int) bool { return d.Extensions[i].Name < d.Extensions[j].Name })
	sort.Slice(d.Schemas, func(i, j int) bool { return d.Schemas[i].Name < d.Schemas[j].Name })
	for _, s := range d.Schemas {
		s.sort()
	}
}

func (s *Schema) sort() {
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	sort.Slice(s.Views, func(i, j int) bool { return s.Views[i].Name < s.Views[j].Name })
	sort.Slice(s.MatViews, func(i, j int) bool { return s.MatViews[i].Name < s.MatViews[j].Name })
	sort.Slice(s.Sequences, func(i, j int) bool { return s.Sequences[i].Name < s.Sequences[j].Name })
	sort.Slice(s.Enums, func(i, j int) bool { return s.Enums[i].Name < s.Enums[j].Name })
	sort.Slice(s.Domains, func(i, j int) bool { return s.Domains[i].Name < s.Domains[j].Name })
	sort.Slice(s.Composites, func(i, j int) bool { return s.Composites[i].Name < s.Composites[j].Name })
	sort.Slice(s.Collations, func(i, j int) bool { return s.Collations[i].Name < s.Collations[j].Name })
	sort.Slice(s.Functions, func(i, j int) bool { return s.Functions[i].sortKey() < s.Functions[j].sortKey() })
	for _, t := range s.Tables {
		t.sort()
	}
	for _, v := range s.Views {
		sort.Slice(v.Triggers, func(i, j int) bool { return v.Triggers[i].Name < v.Triggers[j].Name })
	}
	for _, m := range s.MatViews {
		sort.Slice(m.Indexes, func(i, j int) bool { return m.Indexes[i].Name < m.Indexes[j].Name })
	}
}
