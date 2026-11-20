package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

func indexedTable(defs map[string]string) *model.Database {
	tbl := &model.Table{Schema: "app", Name: "t", Columns: []*model.Column{{Name: "a", Type: "integer"}, {Name: "b", Type: "integer"}}}
	for _, name := range []string{"t_a", "t_a_pgdac_new"} { // deterministic order
		if def, ok := defs[name]; ok {
			tbl.Indexes = append(tbl.Indexes, &model.Index{Name: name, Table: "t", Definition: def})
		}
	}
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{tbl}}}}
}

// TestDropPolicy_CleanupIsNeverKept: an index left under its rebuild name by
// an interrupted run is pg-dac's own leftover, so it is removed even when the
// drop policy keeps indexes the source does not declare — and a table rebuild
// does not carry it across either.
func TestDropPolicy_CleanupIsNeverKept(t *testing.T) {
	const oldDef = "CREATE INDEX t_a ON app.t USING btree (a)"
	const newDef = "CREATE INDEX t_a ON app.t USING btree (a) WHERE (b > 0)"
	from := indexedTable(map[string]string{"t_a": oldDef, "t_a_pgdac_new": newDef})
	to := indexedTable(map[string]string{"t_a": newDef})

	opts := DefaultOptions()
	opts.DropIndexesNotInSource = false
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	var cleanup, plain int
	for _, op := range res.ChangeSet.Ops {
		if op.Type == diff.OpDropIndex {
			if op.Cleanup {
				cleanup++
			} else {
				plain++
			}
		}
	}
	if cleanup != 1 || plain != 0 {
		t.Errorf("want exactly the cleanup drop under a keep policy, got cleanup=%d plain=%d:\n%s", cleanup, plain, res.Script(true))
	}
	if !strings.Contains(res.Script(true), `DROP INDEX CONCURRENTLY "app"."t_a_pgdac_new";`) {
		t.Errorf("cleanup drop missing from the script:\n%s", res.Script(true))
	}

	// Under a column-order rebuild the leftover goes with the old table instead
	// of being recreated on the new one.
	swapped := indexedTable(map[string]string{"t_a": newDef})
	swapped.Schemas[0].Tables[0].Columns = []*model.Column{{Name: "b", Type: "integer"}, {Name: "a", Type: "integer"}}
	opts.ColumnOrder = diff.ColumnOrderRebuild
	res, err = Compare(from, swapped, opts)
	if err != nil {
		t.Fatal(err)
	}
	if script := res.Script(true); strings.Contains(script, "t_a_pgdac_new") {
		t.Errorf("a leftover must not be carried across a rebuild:\n%s", script)
	}
}

// TestObjectChanges_ReasonJoinsDistinctOpReasons: an object touched by several
// ops for different reasons reports each once — here a table rebuilt for its
// column order whose constraint the drop policy keeps across the rebuild.
func TestObjectChanges_ReasonJoinsDistinctOpReasons(t *testing.T) {
	mk := func(colOrder []string, withCheck bool) *model.Database {
		tbl := &model.Table{Schema: "app", Name: "t"}
		for _, c := range colOrder {
			tbl.Columns = append(tbl.Columns, &model.Column{Name: c, Type: "integer"})
		}
		if withCheck {
			tbl.Constraints = []*model.Constraint{{Name: "t_a_check", Type: model.ConstraintCheck, Definition: "CHECK ((a > 0))"}}
		}
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{tbl}}}}
	}
	opts := DefaultOptions()
	opts.ColumnOrder = diff.ColumnOrderRebuild
	opts.DropConstraintsNotInSource = false
	res, err := Compare(mk([]string{"a", "b"}, true), mk([]string{"b", "a"}, false), opts)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := res.ObjectChanges()
	if err != nil {
		t.Fatal(err)
	}
	key := model.ObjRef{Kind: model.KindTable, Schema: "app", Name: "t"}.Key()
	for _, c := range changes {
		if c.ID != key {
			continue
		}
		parts := strings.Split(c.Reason, "; ")
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "table rebuilt to reach the declared column order") || !strings.HasPrefix(parts[1], "kept across the rebuild") {
			t.Errorf("table reason = %q, want the rebuild reason and the carry-over reason, each once", c.Reason)
		}
		return
	}
	t.Fatalf("no change for the table in %v", changes)
}
