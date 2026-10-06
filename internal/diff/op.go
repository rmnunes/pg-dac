// Package diff compares two schema models — a "from" (current/target) model and
// a "to" (desired) model — and produces a typed ChangeSet of structured
// operations that would transform "from" into "to". The diff is purely
// name-keyed and produces no SQL: ordering is the planner's job (internal/plan)
// and rendering is sqlgen's job (internal/sqlgen). Keeping the three concerns
// separate is what lets the same ChangeSet drive a script, a report, and an apply.
package diff

import "github.com/rodrigonunes/pg-dac/internal/model"

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

	OpCreateCollation OpType = "createCollation"
	OpDropCollation   OpType = "dropCollation"

	OpCreateSequence OpType = "createSequence"
	OpDropSequence   OpType = "dropSequence"
	OpAlterSequence  OpType = "alterSequence"

	OpCreateTable OpType = "createTable"
	OpDropTable   OpType = "dropTable"

	OpAddColumn           OpType = "addColumn"
	OpDropColumn          OpType = "dropColumn"
	OpAlterColumnType     OpType = "alterColumnType"
	OpSetColumnDefault    OpType = "setColumnDefault"
	OpDropColumnDefault   OpType = "dropColumnDefault"
	OpSetColumnNotNull    OpType = "setColumnNotNull"
	OpDropColumnNotNull   OpType = "dropColumnNotNull"
	OpAlterColumnIdentity OpType = "alterColumnIdentity"

	OpAddConstraint      OpType = "addConstraint"
	OpDropConstraint     OpType = "dropConstraint"
	OpValidateConstraint OpType = "validateConstraint"

	OpCreateIndex OpType = "createIndex"
	OpDropIndex   OpType = "dropIndex"

	OpCreateView    OpType = "createView" // emitted as CREATE OR REPLACE VIEW
	OpDropView      OpType = "dropView"
	OpCreateMatView OpType = "createMatView"
	OpDropMatView   OpType = "dropMatView"

	OpCreateFunction OpType = "createFunction" // emitted as CREATE OR REPLACE
	OpDropFunction   OpType = "dropFunction"

	OpCreateTrigger OpType = "createTrigger"
	OpDropTrigger   OpType = "dropTrigger"

	OpCreatePolicy OpType = "createPolicy"
	OpDropPolicy   OpType = "dropPolicy"

	OpEnableRLS  OpType = "enableRowLevelSecurity"
	OpDisableRLS OpType = "disableRowLevelSecurity"
)

// IsDrop reports whether the op type drops an object. The planner orders drops
// in reverse dependency order relative to creates.
func (t OpType) IsDrop() bool {
	switch t {
	case OpDropSchema, OpDropExtension, OpDropEnum, OpDropCollation, OpDropSequence, OpDropTable,
		OpDropColumn, OpDropConstraint, OpDropIndex, OpDropView, OpDropMatView,
		OpDropFunction, OpDropTrigger, OpDropPolicy:
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
	Schema     *model.Schema
	Extension  *model.Extension
	PrevExt    *model.Extension
	Enum       *model.EnumType
	Collation  *model.Collation
	Sequence   *model.Sequence
	PrevSeq    *model.Sequence
	Table      *model.Table
	Column     *model.Column
	PrevColumn *model.Column
	Constraint *model.Constraint
	Index      *model.Index
	View       *model.View
	MatView    *model.MaterializedView
	Function   *model.Function
	Trigger    *model.Trigger
	Policy     *model.Policy

	// EnumValues holds the new labels for OpAddEnumValue.
	EnumValues []string
}

// ChangeSet is the ordered-by-discovery list of operations a diff produced.
// The planner consumes it and returns a dependency-correct ordering.
type ChangeSet struct {
	Ops []Op
}

// Empty reports whether the change set contains no operations.
func (c *ChangeSet) Empty() bool { return len(c.Ops) == 0 }

func (c *ChangeSet) add(op Op) { c.Ops = append(c.Ops, op) }
