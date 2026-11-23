package model

import "sort"

// Table models a PostgreSQL table (relkind 'r', or 'p' for a partitioned table).
type Table struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner,omitempty"`
	Comment string `json:"comment,omitempty"`

	// Persistence is "permanent", "unlogged", or "temporary".
	Persistence string `json:"persistence,omitempty"`

	// PartitionStrategy is non-empty ("range"/"list"/"hash") for partitioned
	// tables (relkind 'p'). PartitionKey is the canonical pg_get_partkeydef
	// output, e.g. "RANGE (d)", rendered as PARTITION BY <PartitionKey>.
	PartitionStrategy string `json:"partitionStrategy,omitempty"`
	PartitionKey      string `json:"partitionKey,omitempty"`
	// PartitionOf is set for a partition (relispartition): its parent and the
	// FOR VALUES bound. A partition inherits columns, constraints, indexes and
	// triggers from the parent, so only its own (local) objects are modeled.
	PartitionOf *PartitionParent `json:"partitionOf,omitempty"`

	// RelOptions captures storage parameters (e.g. fillfactor, autovacuum_*).
	RelOptions map[string]string `json:"relOptions,omitempty"`

	// RLSEnabled / RLSForced mirror ALTER TABLE ... ENABLE/FORCE ROW LEVEL SECURITY.
	RLSEnabled bool `json:"rlsEnabled,omitempty"`
	RLSForced  bool `json:"rlsForced,omitempty"`

	// Grants are the privileges roles hold on this object (see Grant).
	Grants []Grant `json:"grants,omitempty"`

	Columns     []*Column     `json:"columns"`
	Constraints []*Constraint `json:"constraints,omitempty"`
	Indexes     []*Index      `json:"indexes,omitempty"`
	Triggers    []*Trigger    `json:"triggers,omitempty"`
	Policies    []*Policy     `json:"policies,omitempty"`
}

// PartitionParent names the partitioned table a partition belongs to and the
// bound it was created with ("FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')",
// "FOR VALUES IN ('a')", "FOR VALUES WITH (MODULUS 4, REMAINDER 0)", or "DEFAULT").
type PartitionParent struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Bound  string `json:"bound"`
}

// Ref returns a dependency-graph reference to this table.
func (t *Table) Ref() ObjRef { return ObjRef{Kind: KindTable, Schema: t.Schema, Name: t.Name} }

// ParentRef returns the partition parent's reference, or false for a non-partition.
func (t *Table) ParentRef() (ObjRef, bool) {
	if t.PartitionOf == nil {
		return ObjRef{}, false
	}
	return ObjRef{Kind: KindTable, Schema: t.PartitionOf.Schema, Name: t.PartitionOf.Table}, true
}

// QualifiedName returns the schema-qualified, quoted table name.
func (t *Table) QualifiedName() string { return QualifiedName(t.Schema, t.Name) }

// Column looks up a column by name, returning nil if absent.
func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (t *Table) sort() {
	// Column order is intentionally NOT sorted: physical column order is
	// significant in PostgreSQL and cannot be changed, so we preserve attnum order.
	sort.Slice(t.Constraints, func(i, j int) bool { return t.Constraints[i].Name < t.Constraints[j].Name })
	sort.Slice(t.Indexes, func(i, j int) bool { return t.Indexes[i].Name < t.Indexes[j].Name })
	sort.Slice(t.Triggers, func(i, j int) bool { return t.Triggers[i].Name < t.Triggers[j].Name })
	sort.Slice(t.Policies, func(i, j int) bool { return t.Policies[i].Name < t.Policies[j].Name })
	SortGrants(t.Grants)
	for _, c := range t.Columns {
		SortGrants(c.Grants)
	}
}

// Trigger models a user trigger on a table (pg_trigger, excluding internal
// constraint triggers).
type Trigger struct {
	Name  string `json:"name"`
	Table string `json:"table"`
	// Definition is the canonical pg_get_triggerdef output (a full CREATE TRIGGER
	// statement). Authoritative equality key.
	Definition string `json:"definition"`
	Comment    string `json:"comment,omitempty"`
	// Enabled is "" (ENABLE, the default), "disabled", "replica", or "always"
	// (ALTER TABLE ... DISABLE / ENABLE REPLICA / ENABLE ALWAYS TRIGGER).
	Enabled string `json:"enabled,omitempty"`
	// DependsOn names the trigger function (so the planner creates the function
	// first even when that function is itself waiting on a later object).
	DependsOn []ObjRef `json:"dependsOn,omitempty"`
}

// Policy models a row-level-security policy (pg_policy).
type Policy struct {
	Name  string `json:"name"`
	Table string `json:"table"`
	// Permissive is true for PERMISSIVE (the default), false for RESTRICTIVE.
	Permissive bool `json:"permissive"`
	// Command is ALL | SELECT | INSERT | UPDATE | DELETE.
	Command string `json:"command"`
	// Roles are the role names the policy applies to ("PUBLIC" when unset).
	Roles []string `json:"roles,omitempty"`
	// Using and WithCheck are the canonical expression texts (may be empty).
	Using     string `json:"using,omitempty"`
	WithCheck string `json:"withCheck,omitempty"`
	Comment   string `json:"comment,omitempty"`
	// DependsOn lists the user functions USING / WITH CHECK call: the policy
	// must be dropped before such a function is, and created after it.
	DependsOn []ObjRef `json:"dependsOn,omitempty"`
}

// Column models a single column of a table (pg_attribute + pg_attrdef).
type Column struct {
	Name string `json:"name"`
	// AttNum is the 1-based physical column position (pg_attribute.attnum). It is
	// preserved for fidelity but is never used as an actionable diff target —
	// PostgreSQL cannot reorder columns.
	AttNum int `json:"attNum,omitempty"`

	// Type is the canonical, fully-resolved type name including modifiers
	// (e.g. "integer", "character varying(255)", "numeric(10,2)", "myschema.mytype").
	Type string `json:"type"`

	NotNull bool `json:"notNull,omitempty"`

	// Default is the canonical default expression text (pg_get_expr of adbin),
	// or "" if there is none.
	Default string `json:"default,omitempty"`
	// DefaultDependsOn lists the user functions the default calls: the default
	// must be dropped before such a function is, and set after it is created.
	DefaultDependsOn []ObjRef `json:"defaultDependsOn,omitempty"`

	// Identity is "" (none), "always" (GENERATED ALWAYS AS IDENTITY), or
	// "byDefault" (GENERATED BY DEFAULT AS IDENTITY).
	Identity string `json:"identity,omitempty"`

	// Generated is "" (none), "stored", or "virtual" (PG18+). GeneratedExpr holds
	// the generation expression when Generated != "".
	Generated     string `json:"generated,omitempty"`
	GeneratedExpr string `json:"generatedExpr,omitempty"`

	// Collation is the non-default collation name (schema-qualified), or "".
	Collation string `json:"collation,omitempty"`

	// Storage attributes. Empty strings mean "type default".
	Storage     string `json:"storage,omitempty"`     // plain|external|extended|main
	Compression string `json:"compression,omitempty"` // pglz|lz4 (PG14+)
	StatsTarget *int   `json:"statsTarget,omitempty"` // per-column statistics target

	Comment string `json:"comment,omitempty"`

	// Grants are column-level privileges (GRANT SELECT (col) ON ...).
	Grants []Grant `json:"grants,omitempty"`
}

// ConstraintType enumerates the kinds of table constraints pg-dac models.
type ConstraintType string

const (
	ConstraintPrimaryKey ConstraintType = "primaryKey"
	ConstraintForeignKey ConstraintType = "foreignKey"
	ConstraintUnique     ConstraintType = "unique"
	ConstraintCheck      ConstraintType = "check"
	ConstraintExclude    ConstraintType = "exclude"
)

// Constraint models a table constraint (pg_constraint).
type Constraint struct {
	Name string         `json:"name"`
	Type ConstraintType `json:"type"`

	// Definition is the canonical pg_get_constraintdef output WITHOUT the
	// leading "CONSTRAINT name " — i.e. the body such as
	// "PRIMARY KEY (id)" or "FOREIGN KEY (a) REFERENCES other(b)". This is the
	// authoritative equality key; structured fields below support ordering and
	// safe staged application (NOT VALID / VALIDATE).
	Definition string `json:"definition"`

	// Columns are the local columns participating (PK/UNIQUE/FK), in order.
	Columns []string `json:"columns,omitempty"`

	// Foreign-key specifics (Type == ConstraintForeignKey).
	RefSchema  string   `json:"refSchema,omitempty"`
	RefTable   string   `json:"refTable,omitempty"`
	RefColumns []string `json:"refColumns,omitempty"`

	// Validated is false for a constraint added but not yet validated (NOT VALID).
	// pg-dac normally adds FK/CHECK constraints as NOT VALID then VALIDATEs them.
	Validated bool `json:"validated"`

	Deferrable        bool `json:"deferrable,omitempty"`
	InitiallyDeferred bool `json:"initiallyDeferred,omitempty"`

	Comment string `json:"comment,omitempty"`

	// DependsOn lists the user functions a CHECK constraint calls: it must be
	// dropped before such a function is, and added after it is created.
	DependsOn []ObjRef `json:"dependsOn,omitempty"`
}

// Ref returns a dependency-graph reference to the referenced table of a foreign
// key, or the zero ObjRef for non-FK constraints.
func (c *Constraint) ForeignTableRef() (ObjRef, bool) {
	if c.Type != ConstraintForeignKey || c.RefTable == "" {
		return ObjRef{}, false
	}
	return ObjRef{Kind: KindTable, Schema: c.RefSchema, Name: c.RefTable}, true
}

// Index models a secondary index (pg_index / pg_class). Primary-key and unique
// constraints have backing indexes that are modeled with their constraint, not
// here; this models standalone CREATE INDEX objects.
type Index struct {
	Name  string `json:"name"`
	Table string `json:"table"`

	// Definition is the canonical pg_get_indexdef output (a full CREATE INDEX
	// statement). It is the authoritative equality key.
	Definition string `json:"definition"`

	IsUnique bool `json:"isUnique,omitempty"`

	// Columns/expressions, best-effort, for reporting.
	Columns []string `json:"columns,omitempty"`

	// Method is the index access method (btree, gin, gist, hash, ...).
	Method string `json:"method,omitempty"`

	Comment string `json:"comment,omitempty"`

	// DependsOn lists the user functions the index's expressions or predicate
	// call: it must be dropped before such a function is, and built after it.
	DependsOn []ObjRef `json:"dependsOn,omitempty"`
}
