package diff

import (
	"reflect"
	"testing"

	"github.com/rodrigonunes/pg-dac/internal/model"
)

func dbWith(s *model.Schema) *model.Database { return &model.Database{Schemas: []*model.Schema{s}} }

func enumDB(vals ...string) *model.Database {
	return dbWith(&model.Schema{Name: "public", Enums: []*model.EnumType{{Schema: "public", Name: "e", Values: vals}}})
}

func TestDiff_EnumValuePositions(t *testing.T) {
	cs := Diff(enumDB("a", "c"), enumDB("a", "b", "c", "d"))
	if len(cs.Ops) != 1 || cs.Ops[0].Type != OpAddEnumValue {
		t.Fatalf("expected one addEnumValue op, got %v", opSummary(cs))
	}
	op := cs.Ops[0]
	if !reflect.DeepEqual(op.EnumValues, []string{"b", "d"}) ||
		!reflect.DeepEqual(op.EnumPositions, []EnumPosition{{After: "a"}, {After: "c"}}) {
		t.Errorf("values %v positions %+v", op.EnumValues, op.EnumPositions)
	}
	cs = Diff(enumDB("b", "c"), enumDB("a", "b", "c"))
	if p := cs.Ops[0].EnumPositions; len(p) != 1 || p[0].Before != "b" {
		t.Errorf("prepended value should go BEFORE the first existing one, got %+v", p)
	}
	if cs := Diff(enumDB("a", "b"), enumDB("a")); !hasOp(cs, OpUnsupported, "e") {
		t.Errorf("removing a value must be an unsupported change, got %v", opSummary(cs))
	}
	if cs := Diff(enumDB("a", "b"), enumDB("b", "a")); !hasOp(cs, OpUnsupported, "e") {
		t.Errorf("reordering values must be an unsupported change, got %v", opSummary(cs))
	}
	if cs := Diff(enumDB("a", "b"), enumDB("a", "b")); !cs.Empty() {
		t.Errorf("identical enums: %v", opSummary(cs))
	}
}

func tableDB(mod func(t *model.Table)) *model.Database {
	tbl := &model.Table{Schema: "public", Name: "t",
		Columns:  []*model.Column{{Name: "a", Type: "integer"}, {Name: "b", Type: "text"}},
		Indexes:  []*model.Index{{Name: "t_a_idx", Table: "t", Definition: "CREATE INDEX t_a_idx ON public.t USING btree (a)"}},
		Triggers: []*model.Trigger{{Name: "tg", Table: "t", Definition: "CREATE TRIGGER tg BEFORE INSERT ON public.t FOR EACH ROW EXECUTE FUNCTION public.f()"}},
	}
	if mod != nil {
		mod(tbl)
	}
	return dbWith(&model.Schema{Name: "public", Tables: []*model.Table{tbl}})
}

func TestDiff_Comments(t *testing.T) {
	cs := Diff(tableDB(nil), tableDB(func(tb *model.Table) {
		tb.Comment = "table"
		tb.Columns[0].Comment = "col"
		tb.Indexes[0].Comment = "idx"
		tb.Triggers[0].Comment = "trg"
	}))
	want := map[string]bool{"": false, "column": false, "index": false, "trigger": false}
	for _, op := range cs.Ops {
		if op.Type != OpSetComment {
			t.Errorf("unexpected op %s", op.Type)
			continue
		}
		want[op.CommentKind] = true
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("missing comment op for kind %q", kind)
		}
	}
	// Removing a comment is a change too.
	cs = Diff(tableDB(func(tb *model.Table) { tb.Comment = "x" }), tableDB(nil))
	if len(cs.Ops) != 1 || cs.Ops[0].Type != OpSetComment || cs.Ops[0].Comment != "" {
		t.Errorf("comment removal: %v", opSummary(cs))
	}
}

func TestDiff_TableAttributes(t *testing.T) {
	cs := Diff(tableDB(nil), tableDB(func(tb *model.Table) { tb.Persistence = "unlogged" }))
	if !hasOp(cs, OpSetTableLogged, "t") {
		t.Errorf("persistence change: %v", opSummary(cs))
	}
	cs = Diff(tableDB(func(tb *model.Table) {
		tb.RelOptions = map[string]string{"fillfactor": "70", "autovacuum_enabled": "false"}
	}),
		tableDB(func(tb *model.Table) { tb.RelOptions = map[string]string{"fillfactor": "90"} }))
	if len(cs.Ops) != 1 || cs.Ops[0].Type != OpSetRelOptions {
		t.Fatalf("reloptions: %v", opSummary(cs))
	}
	if cs.Ops[0].RelOptions["fillfactor"] != "90" || !reflect.DeepEqual(cs.Ops[0].RelOptionsReset, []string{"autovacuum_enabled"}) {
		t.Errorf("set %v reset %v", cs.Ops[0].RelOptions, cs.Ops[0].RelOptionsReset)
	}
	n := 500
	cs = Diff(tableDB(nil), tableDB(func(tb *model.Table) {
		tb.Columns[1].Storage = "external"
		tb.Columns[1].Compression = "lz4"
		tb.Columns[0].StatsTarget = &n
	}))
	for _, want := range []OpType{OpSetColumnStorage, OpSetColumnCompression, OpSetColumnStats} {
		if !hasOp(cs, want, "t") {
			t.Errorf("missing %s in %v", want, opSummary(cs))
		}
	}
	// A new table carries its attribute ops too.
	cs = Diff(dbWith(&model.Schema{Name: "public"}), tableDB(func(tb *model.Table) { tb.Columns[1].Storage = "external" }))
	if !hasOp(cs, OpCreateTable, "t") || !hasOp(cs, OpSetColumnStorage, "t") {
		t.Errorf("new table with storage attr: %v", opSummary(cs))
	}
	// Generated expression change.
	cs = Diff(tableDB(func(tb *model.Table) { tb.Columns[1].Generated, tb.Columns[1].GeneratedExpr = "stored", "(a * 2)" }),
		tableDB(func(tb *model.Table) { tb.Columns[1].Generated, tb.Columns[1].GeneratedExpr = "stored", "(a * 3)" }))
	if len(cs.Ops) != 1 || cs.Ops[0].Type != OpAlterColumnGenerated {
		t.Errorf("generated expr change: %v", opSummary(cs))
	}
}

func TestDiff_TriggerEnabledState(t *testing.T) {
	cs := Diff(tableDB(nil), tableDB(func(tb *model.Table) { tb.Triggers[0].Enabled = "disabled" }))
	if len(cs.Ops) != 1 || cs.Ops[0].Type != OpSetTriggerEnabled {
		t.Errorf("disable existing trigger: %v", opSummary(cs))
	}
	cs = Diff(dbWith(&model.Schema{Name: "public"}), tableDB(func(tb *model.Table) { tb.Triggers[0].Enabled = "always" }))
	if !hasOp(cs, OpCreateTrigger, "t") || !hasOp(cs, OpSetTriggerEnabled, "t") {
		t.Errorf("new trigger with non-default state: %v", opSummary(cs))
	}
}

func TestDiff_ViewTriggersAndMatViewIndexes(t *testing.T) {
	view := func(cols []model.ViewColumn, def string) *model.Database {
		return dbWith(&model.Schema{Name: "public", Views: []*model.View{{
			Schema: "public", Name: "v", Definition: def, Columns: cols,
			Triggers: []*model.Trigger{{Name: "vt", Table: "v", Definition: "CREATE TRIGGER vt INSTEAD OF INSERT ON public.v FOR EACH ROW EXECUTE FUNCTION public.f()"}},
		}}})
	}
	// Incompatible column change: view recreated, its trigger created afresh, no trigger drop.
	cs := Diff(view([]model.ViewColumn{{Name: "a", Type: "integer"}, {Name: "b", Type: "text"}}, "SELECT a, b FROM public.t"),
		view([]model.ViewColumn{{Name: "a", Type: "integer"}}, "SELECT a FROM public.t"))
	if !hasOp(cs, OpDropView, "v") || !hasOp(cs, OpCreateView, "v") || !hasOp(cs, OpCreateTrigger, "v") || hasOp(cs, OpDropTrigger, "v") {
		t.Errorf("recreated view triggers: %v", opSummary(cs))
	}
	// New view with a trigger.
	cs = Diff(dbWith(&model.Schema{Name: "public"}), view(nil, "SELECT a FROM public.t"))
	if !hasOp(cs, OpCreateView, "v") || !hasOp(cs, OpCreateTrigger, "v") {
		t.Errorf("new view triggers: %v", opSummary(cs))
	}

	mv := func(def string) *model.Database {
		return dbWith(&model.Schema{Name: "public", MatViews: []*model.MaterializedView{{
			Schema: "public", Name: "mv", Definition: def, WithData: true,
			Indexes: []*model.Index{{Name: "mv_idx", Table: "mv", Definition: "CREATE INDEX mv_idx ON public.mv USING btree (a)"}},
		}}})
	}
	cs = Diff(mv("SELECT a FROM public.t"), mv("SELECT a FROM public.u"))
	if !hasOp(cs, OpDropMatView, "mv") || !hasOp(cs, OpCreateMatView, "mv") || !hasOp(cs, OpCreateIndex, "mv") || hasOp(cs, OpDropIndex, "mv") {
		t.Errorf("recreated matview indexes: %v", opSummary(cs))
	}
	if cs := Diff(mv("SELECT a FROM public.t"), mv("SELECT a FROM public.t")); !cs.Empty() {
		t.Errorf("identical matviews: %v", opSummary(cs))
	}
}

func TestDiff_PartitionsDomainsComposites(t *testing.T) {
	child := func(bound string) *model.Database {
		parent := &model.Table{Schema: "public", Name: "m", Columns: []*model.Column{{Name: "d", Type: "date"}}, PartitionStrategy: "range", PartitionKey: "RANGE (d)"}
		c := &model.Table{Schema: "public", Name: "m_1", PartitionOf: &model.PartitionParent{Schema: "public", Table: "m", Bound: bound}}
		return dbWith(&model.Schema{Name: "public", Tables: []*model.Table{parent, c}})
	}
	cs := Diff(child("FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')"), child("FOR VALUES FROM ('2024-01-01') TO ('2026-01-01')"))
	if !hasOp(cs, OpDropTable, "m_1") || !hasOp(cs, OpCreateTable, "m_1") {
		t.Errorf("re-bound partition must be recreated: %v", opSummary(cs))
	}
	if cs := Diff(child("DEFAULT"), child("DEFAULT")); !cs.Empty() {
		t.Errorf("identical partitions: %v", opSummary(cs))
	}

	dom := func(def string) *model.Database {
		return dbWith(&model.Schema{Name: "public", Domains: []*model.Domain{{Schema: "public", Name: "d", Definition: def}}})
	}
	if cs := Diff(dbWith(&model.Schema{Name: "public"}), dom("CREATE DOMAIN d AS text")); !hasOp(cs, OpCreateDomain, "d") {
		t.Errorf("new domain: %v", opSummary(cs))
	}
	if cs := Diff(dom("CREATE DOMAIN d AS text"), dbWith(&model.Schema{Name: "public"})); !hasOp(cs, OpDropDomain, "d") {
		t.Errorf("dropped domain: %v", opSummary(cs))
	}
	if cs := Diff(dom("CREATE DOMAIN d AS text"), dom("CREATE DOMAIN d AS text NOT NULL")); !hasOp(cs, OpUnsupported, "d") {
		t.Errorf("changed domain must be an unsupported change: %v", opSummary(cs))
	}
	comp := func(def string) *model.Database {
		return dbWith(&model.Schema{Name: "public", Composites: []*model.CompositeType{{Schema: "public", Name: "c", Definition: def}}})
	}
	if cs := Diff(dbWith(&model.Schema{Name: "public"}), comp("CREATE TYPE c AS (x integer)")); !hasOp(cs, OpCreateCompositeType, "c") {
		t.Errorf("new composite: %v", opSummary(cs))
	}
	if cs := Diff(comp("CREATE TYPE c AS (x integer)"), comp("CREATE TYPE c AS (x bigint)")); !hasOp(cs, OpUnsupported, "c") {
		t.Errorf("changed composite must be an unsupported change: %v", opSummary(cs))
	}
}
