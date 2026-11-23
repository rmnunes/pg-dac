// Package diff compares two schema models — a "from" (current/target) model and
// a "to" (desired) model — and produces a typed ChangeSet of structured
// operations that would transform "from" into "to". The diff is purely
// name-keyed and produces no SQL: ordering is the planner's job (internal/plan)
// and rendering is sqlgen's job (internal/sqlgen). Keeping the three concerns
// separate is what lets the same ChangeSet drive a script, a report, and an apply.
package diff

import "github.com/rmnunes/pg-dac/internal/model"

// OpType enumerates the concrete schema-change operations pg-dac emits. Each
// value maps to one (sometimes a small staged sequence of) SQL statements in
// sqlgen and to a hazard classification in internal/safety.
type OpType string

const (
	OpCreateSchema OpType = "createSchema"
	OpDropSchema   OpType = "dropSchema"

	OpCreateExtension OpType = "createExtension"
	OpDropExtension   OpType = "dropExtension"
	OpAlterExtension  OpType = "alterExtension" // ALTER EXTENSION ... UPDATE TO

	OpCreateEnum   OpType = "createEnum"
	OpDropEnum     OpType = "dropEnum"
	OpAddEnumValue OpType = "addEnumValue"

	OpCreateDomain        OpType = "createDomain"
	OpDropDomain          OpType = "dropDomain"
	OpCreateCompositeType OpType = "createCompositeType"
	OpDropCompositeType   OpType = "dropCompositeType"

	OpCreateCollation OpType = "createCollation"
	OpDropCollation   OpType = "dropCollation"

	OpCreateSequence   OpType = "createSequence"
	OpDropSequence     OpType = "dropSequence"
	OpAlterSequence    OpType = "alterSequence"
	OpSetSequenceOwner OpType = "alterSequenceOwnedBy" // ALTER SEQUENCE ... OWNED BY (after the owning table exists)

	OpCreateTable    OpType = "createTable"
	OpDropTable      OpType = "dropTable"
	OpSetTableLogged OpType = "setTableLogged" // ALTER TABLE ... SET LOGGED / SET UNLOGGED
	OpSetRelOptions  OpType = "setRelOptions"  // ALTER TABLE ... SET (...) / RESET (...)

	OpAddColumn            OpType = "addColumn"
	OpDropColumn           OpType = "dropColumn"
	OpAlterColumnType      OpType = "alterColumnType"
	OpSetColumnDefault     OpType = "setColumnDefault"
	OpDropColumnDefault    OpType = "dropColumnDefault"
	OpSetColumnNotNull     OpType = "setColumnNotNull"
	OpDropColumnNotNull    OpType = "dropColumnNotNull"
	OpAlterColumnIdentity  OpType = "alterColumnIdentity"
	OpAlterColumnGenerated OpType = "alterColumnGenerated" // SET EXPRESSION / DROP EXPRESSION, or drop + re-add
	OpSetColumnStorage     OpType = "setColumnStorage"
	OpSetColumnCompression OpType = "setColumnCompression"
	OpSetColumnStats       OpType = "setColumnStatistics"

	OpAddConstraint      OpType = "addConstraint"
	OpDropConstraint     OpType = "dropConstraint"
	OpValidateConstraint OpType = "validateConstraint"

	OpCreateIndex OpType = "createIndex"
	OpDropIndex   OpType = "dropIndex"
	// OpReplaceIndex rebuilds an index whose definition changed (PostgreSQL
	// cannot alter one in place); online it is built under a temporary name and
	// swapped in so the table is never without it — see sqlgen.renderReplaceIndex.
	OpReplaceIndex OpType = "replaceIndex"

	OpCreateView    OpType = "createView" // emitted as CREATE OR REPLACE VIEW
	OpDropView      OpType = "dropView"
	OpCreateMatView OpType = "createMatView"
	OpDropMatView   OpType = "dropMatView"

	OpCreateFunction OpType = "createFunction" // emitted as CREATE OR REPLACE
	OpDropFunction   OpType = "dropFunction"

	OpCreateTrigger     OpType = "createTrigger"
	OpDropTrigger       OpType = "dropTrigger"
	OpSetTriggerEnabled OpType = "setTriggerEnabled" // ALTER TABLE ... DISABLE / ENABLE [REPLICA|ALWAYS] TRIGGER

	OpCreatePolicy OpType = "createPolicy"
	OpDropPolicy   OpType = "dropPolicy"

	OpEnableRLS  OpType = "enableRowLevelSecurity"
	OpDisableRLS OpType = "disableRowLevelSecurity"

	// OpSetComment changes the comment of an existing object or sub-object
	// (CommentKind + Sub name it; Comment "" removes it).
	OpSetComment OpType = "setComment"

	// OpGrant / OpRevoke change the privileges a grantee holds on an object (or,
	// with Sub set, on a column). Op.Grant carries the grantee and the privileges
	// concerned: for OpGrant each privilege's Grantable is the state to reach
	// (WITH GRANT OPTION or not); for OpRevoke a privilege with Grantable=true
	// means "revoke only the grant option" (REVOKE GRANT OPTION FOR), false the
	// privilege itself.
	OpGrant  OpType = "grant"
	OpRevoke OpType = "revoke"

	// OpGrantDefault / OpRevokeDefault change a default-privilege entry
	// (ALTER DEFAULT PRIVILEGES): Op.DefaultPriv names the role, schema and
	// object type, Op.Grant the grantee and privileges, as for OpGrant/OpRevoke.
	OpGrantDefault  OpType = "grantDefault"
	OpRevokeDefault OpType = "revokeDefault"

	// OpUnsupported records a desired change PostgreSQL cannot apply in place
	// (dropping/reordering enum values, altering a type in use, re-partitioning).
	// It renders as a RAISE WARNING carrying an error-severity hazard so the
	// schema never silently fails to converge.
	OpUnsupported OpType = "unsupportedChange"
)

// IsDrop reports whether the op type drops an object. The planner orders drops
// in reverse dependency order relative to creates.
func (t OpType) IsDrop() bool {
	switch t {
	case OpDropSchema, OpDropExtension, OpDropEnum, OpDropDomain, OpDropCompositeType,
		OpDropCollation, OpDropSequence, OpDropTable,
		OpDropColumn, OpDropConstraint, OpDropIndex, OpDropView, OpDropMatView,
		OpDropFunction, OpDropTrigger, OpDropPolicy:
		return true
	}
	return false
}

// IsObjectDrop reports whether the op drops a top-level object — as opposed
// to a column, constraint, index, trigger or policy of a relation, whose op
// names the relation as Owner and the sub-object as Sub.
func (t OpType) IsObjectDrop() bool {
	switch t {
	case OpDropSchema, OpDropExtension, OpDropEnum, OpDropDomain, OpDropCompositeType,
		OpDropCollation, OpDropSequence, OpDropTable, OpDropView, OpDropMatView, OpDropFunction:
		return true
	}
	return false
}

// IsObjectCreate reports whether the op creates a top-level object (the
// counterpart of IsObjectDrop).
func (t OpType) IsObjectCreate() bool {
	switch t {
	case OpCreateSchema, OpCreateExtension, OpCreateEnum, OpCreateDomain, OpCreateCompositeType,
		OpCreateCollation, OpCreateSequence, OpCreateTable, OpCreateView, OpCreateMatView, OpCreateFunction:
		return true
	}
	return false
}

// Op is a single structured schema-change operation. Only the payload fields
// relevant to Type are populated; consumers switch on Type.
type Op struct {
	Type OpType

	// Owner identifies the object the op acts on, or — for column/constraint/
	// index operations — the table that contains the sub-object. Sub names the
	// sub-object (column/constraint/index) when applicable.
	Owner model.ObjRef
	Sub   string

	// Payloads.
	Schema      *model.Schema
	Extension   *model.Extension
	PrevExt     *model.Extension
	Enum        *model.EnumType
	Domain      *model.Domain
	Composite   *model.CompositeType
	Collation   *model.Collation
	Sequence    *model.Sequence
	PrevSeq     *model.Sequence
	Table       *model.Table
	PrevTable   *model.Table
	Column      *model.Column
	PrevColumn  *model.Column
	Constraint  *model.Constraint
	Index       *model.Index
	View        *model.View
	MatView     *model.MaterializedView
	Function    *model.Function
	Trigger     *model.Trigger
	PrevTrigger *model.Trigger
	Policy      *model.Policy

	// EnumValues holds the new labels for OpAddEnumValue; EnumPositions (same
	// length) says where each goes (BEFORE/AFTER an existing label; both empty
	// = append).
	EnumValues    []string
	EnumPositions []EnumPosition

	// CommentKind (OpSetComment) is "" for the owning object itself, else
	// "column" | "constraint" | "index" | "trigger" | "policy" with Sub naming it;
	// Comment is the new text ("" removes the comment).
	CommentKind string
	Comment     string

	// RelOptions / RelOptionsReset (OpSetRelOptions) are the storage parameters
	// to SET and to RESET.
	RelOptions      map[string]string
	RelOptionsReset []string

	// Grant (OpGrant / OpRevoke) names the grantee and privileges concerned.
	Grant *model.Grant
	// MissingRole (OpGrant) marks a grantee the target cluster does not have.
	MissingRole bool
	// DefaultPriv (OpGrantDefault / OpRevokeDefault) is the entry changed.
	DefaultPriv *model.DefaultPrivilege
	// MissingForRole (OpGrantDefault) marks an entry for a role the target
	// cluster does not have.
	MissingForRole bool

	// Rebuild (OpCreateTable) marks a table rebuilt in place of PrevTable to
	// reach a declared column order PostgreSQL cannot produce with ALTERs: the
	// new table is created under a temporary name, the rows are copied, the old
	// table dropped and the new one renamed — atomically. OwnedSequences lists
	// the serial-style sequences owned by the table's columns, detached before
	// the drop and re-owned after the rename (they would go with the old table).
	Rebuild        bool
	OwnedSequences []*model.Sequence

	// Cleanup (OpDropIndex) marks the removal of something pg-dac itself left
	// behind — an index still under its rebuild name after an interrupted run
	// (see OpReplaceIndex). The drop policy never keeps such an object.
	Cleanup bool

	// SeedSequence (OpAlterColumnIdentity, OpSetColumnDefault) marks that the
	// column of an EXISTING table gets a value generator that did not exist
	// before — a new identity, or a default drawing from a sequence created by
	// this migration. A fresh sequence starts at 1, so sqlgen seeds it past the
	// column's current maximum; without that the next insert collides.
	SeedSequence bool

	// Reason, when set, explains why this op exists beyond the plain diff (e.g.
	// a view recreated because CREATE OR REPLACE cannot drop its columns, or a
	// dependent recreated around a blocking change). Rendered as a SQL comment.
	Reason string

	// PrevIndex (OpReplaceIndex) is the target's current definition of the index
	// that Index replaces; TempIndexName is the deterministic, collision-free
	// name the replacement is built under before it takes the original name.
	PrevIndex     *model.Index
	TempIndexName string
}

// EnumPosition places a new enum label relative to an existing one.
type EnumPosition struct {
	Before string
	After  string
}

// ChangeSet is the ordered-by-discovery list of operations a diff produced.
// The planner consumes it and returns a dependency-correct ordering.
type ChangeSet struct {
	Ops []Op
	// Warnings are differences the differ saw but — by option or by PostgreSQL's
	// nature — emits no operation for (e.g. a declared column order the target
	// does not have). The engine surfaces them with the migration.
	Warnings []string
}

// Empty reports whether the change set contains no operations.
func (c *ChangeSet) Empty() bool { return len(c.Ops) == 0 }

func (c *ChangeSet) add(op Op) { c.Ops = append(c.Ops, op) }
