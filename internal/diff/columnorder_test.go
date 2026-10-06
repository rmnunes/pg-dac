package diff

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

func cols(names ...string) []*model.Column {
	out := make([]*model.Column, len(names))
	for i, n := range names {
		out[i] = &model.Column{Name: n, Type: "integer"}
	}
	return out
}

func orderDB(t *model.Table, more ...*model.Table) *model.Database {
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: append([]*model.Table{t}, more...)}}}
}

// PostgreSQL appends new columns, so a declared order is reachable only when
// every new column trails every existing one in the declared order; otherwise
// the policy decides: silent, a warning, or a table rebuild.
func TestColumnOrderPolicy(t *testing.T) {
	from := orderDB(&model.Table{Schema: "app", Name: "t", Columns: cols("a", "c")})
	appended := orderDB(&model.Table{Schema: "app", Name: "t", Columns: cols("a", "c", "b")})
	inserted := orderDB(&model.Table{Schema: "app", Name: "t", Columns: cols("a", "b", "c")})
	swapped := orderDB(&model.Table{Schema: "app", Name: "t", Columns: cols("c", "a")})
	dropped := orderDB(&model.Table{Schema: "app", Name: "t", Columns: cols("c")})

	for name, to := range map[string]*model.Database{"appended": appended, "dropped": dropped} {
		cs := DiffWith(from, to, Options{ColumnOrder: ColumnOrderRebuild})
		if len(cs.Warnings) != 0 || containsOp(cs, OpCreateTable) {
			t.Errorf("%s: reachable order must neither warn nor rebuild: %v %v", name, cs.Warnings, opSummary(cs))
		}
	}

	// default (warn): a warning naming the table and both orders, no rebuild
	cs := Diff(from, inserted)
	if len(cs.Warnings) != 1 || !strings.Contains(cs.Warnings[0], `"app"."t"`) || !strings.Contains(cs.Warnings[0], "column b declared") {
		t.Fatalf("warn: got warnings %v", cs.Warnings)
	}
	if containsOp(cs, OpCreateTable) || !containsOp(cs, OpAddColumn) {
		t.Fatalf("warn must still add the column normally: %v", opSummary(cs))
	}
	if cs := DiffWith(from, swapped, Options{ColumnOrder: ColumnOrderWarn}); len(cs.Warnings) != 1 || !cs.Empty() {
		t.Fatalf("swapped order under warn: want a warning and no ops, got %v %v", cs.Warnings, opSummary(cs))
	}

	// ignore: silent
	if cs := DiffWith(from, swapped, Options{ColumnOrder: ColumnOrderIgnore}); len(cs.Warnings) != 0 || !cs.Empty() {
		t.Fatalf("ignore must be silent: %v %v", cs.Warnings, opSummary(cs))
	}

	// rebuild: drop + create pair flagged as a rebuild, no column ops
	cs = DiffWith(from, inserted, Options{ColumnOrder: ColumnOrderRebuild})
	var create *Op
	for i := range cs.Ops {
		if cs.Ops[i].Type == OpCreateTable {
			create = &cs.Ops[i]
		}
	}
	if create == nil || !create.Rebuild || create.PrevTable == nil || !containsOp(cs, OpDropTable) || containsOp(cs, OpAddColumn) {
		t.Fatalf("rebuild: want DropTable + CreateTable{Rebuild, PrevTable}, no column ops; got %v", opSummary(cs))
	}
	if len(cs.Warnings) != 0 {
		t.Fatalf("rebuild must not also warn: %v", cs.Warnings)
	}
}

// A rebuilt table's own sub-objects are created afresh, and foreign keys on
// other tables that reference it are dropped before and re-added after (they
// would otherwise block DROP TABLE). Partitioned tables and partitions cannot
// be rebuilt and surface as unsupported.
func TestColumnOrderRebuildSubObjects(t *testing.T) {
	fk := &model.Constraint{Name: "c_t_fk", Type: model.ConstraintForeignKey, Definition: `FOREIGN KEY (t_id) REFERENCES app.t(a)`,
		Columns: []string{"t_id"}, RefSchema: "app", RefTable: "t", RefColumns: []string{"a"}, Validated: true}
	child := func() *model.Table {
		return &model.Table{Schema: "app", Name: "c", Columns: cols("id", "t_id"), Constraints: []*model.Constraint{fk}}
	}
	mk := func(order ...string) *model.Database {
		tbl := &model.Table{Schema: "app", Name: "t", Columns: cols(order...),
			Constraints: []*model.Constraint{{Name: "t_pkey", Type: model.ConstraintPrimaryKey, Definition: "PRIMARY KEY (a)", Columns: []string{"a"}, Validated: true}},
			Indexes:     []*model.Index{{Name: "t_b", Table: "t", Definition: `CREATE INDEX t_b ON app.t USING btree (b)`}},
			Grants:      []model.Grant{{Grantee: "reader", Privileges: []model.Privilege{{Type: "SELECT"}}}}}
		return orderDB(tbl, child())
	}
	cs := DiffWith(mk("a", "c", "b"), mk("a", "b", "c"), Options{ColumnOrder: ColumnOrderRebuild})
	var fkDrop, fkAdd, pk, idx bool
	for _, op := range cs.Ops {
		switch {
		case op.Type == OpDropConstraint && op.Sub == "c_t_fk":
			fkDrop = true
		case op.Type == OpAddConstraint && op.Sub == "c_t_fk":
			fkAdd = true
		case op.Type == OpAddConstraint && op.Sub == "t_pkey":
			pk = true
		case op.Type == OpCreateIndex && op.Sub == "t_b":
			idx = true
		}
	}
	if !fkDrop || !fkAdd || !pk || !idx {
		t.Fatalf("rebuild must re-add the FK referencing the table and recreate its own constraints/indexes; got %v", opSummary(cs))
	}

	part := orderDB(&model.Table{Schema: "app", Name: "t", Columns: cols("a", "b"), PartitionStrategy: "range", PartitionKey: "RANGE (a)"})
	partSwapped := orderDB(&model.Table{Schema: "app", Name: "t", Columns: cols("b", "a"), PartitionStrategy: "range", PartitionKey: "RANGE (b)"})
	cs = DiffWith(part, partSwapped, Options{ColumnOrder: ColumnOrderRebuild})
	if !containsOp(cs, OpUnsupported) || containsOp(cs, OpCreateTable) {
		t.Fatalf("a partitioned table cannot be rebuilt: want OpUnsupported, got %v", opSummary(cs))
	}
}

func containsOp(cs *ChangeSet, typ OpType) bool {
	for _, op := range cs.Ops {
		if op.Type == typ {
			return true
		}
	}
	return false
}
