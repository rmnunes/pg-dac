package sqlgen

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

func TestRenameIndexDef(t *testing.T) {
	cases := []struct{ def, name, want string }{
		{"CREATE INDEX t_a ON public.t USING btree (a)", "t_a_pgdac_new", "CREATE INDEX t_a_pgdac_new ON public.t USING btree (a)"},
		{"CREATE UNIQUE INDEX t_u ON public.t USING btree (a, b)", "t_u_pgdac_new", "CREATE UNIQUE INDEX t_u_pgdac_new ON public.t USING btree (a, b)"},
		{`CREATE INDEX "My Index" ON public.t USING btree (a)`, "x", "CREATE INDEX x ON public.t USING btree (a)"},
		{`CREATE INDEX "quo""te" ON public.t USING btree (a)`, "x", "CREATE INDEX x ON public.t USING btree (a)"},
	}
	for _, c := range cases {
		// model.QuoteIdent always quotes; compare without the quotes.
		if got := strip(renameIndexDef(c.def, c.name)); got != c.want {
			t.Errorf("renameIndexDef(%q) = %q, want %q", c.def, got, c.want)
		}
	}
}

func replaceOp(partitionKey string) diff.Op {
	tbl := &model.Table{Schema: "public", Name: "t", PartitionKey: partitionKey}
	return diff.Op{
		Type:          diff.OpReplaceIndex,
		Owner:         tbl.Ref(),
		Sub:           "t_a",
		Table:         tbl,
		PrevIndex:     &model.Index{Name: "t_a", Table: "t", Definition: "CREATE INDEX t_a ON public.t USING btree (a)"},
		Index:         &model.Index{Name: "t_a", Table: "t", Definition: "CREATE INDEX t_a ON public.t USING btree (a) WHERE (b IS NOT NULL)", Comment: "hot rows"},
		TempIndexName: "t_a_pgdac_new",
	}
}

// strip removes identifier quoting so the assertions do not depend on the
// quoting policy of model.QualifiedName.
func strip(s string) string { return strings.ReplaceAll(s, `"`, "") }

// TestRenderReplaceIndex_Online: the REINDEX CONCURRENTLY sequence — clear a
// leftover, build the replacement concurrently under the temporary name, drop
// the old index, rename — with no INDEX_DROPPED hazard and the comment on the
// final name.
func TestRenderReplaceIndex_Online(t *testing.T) {
	stmts := renderReplaceIndex(replaceOp(""), Options{Online: true})
	want := []string{
		"DROP INDEX CONCURRENTLY IF EXISTS public.t_a_pgdac_new;",
		"CREATE INDEX CONCURRENTLY t_a_pgdac_new ON public.t USING btree (a) WHERE (b IS NOT NULL);",
		"DROP INDEX CONCURRENTLY IF EXISTS public.t_a;",
		"ALTER INDEX public.t_a_pgdac_new RENAME TO t_a;",
		"COMMENT ON INDEX public.t_a IS 'hot rows';",
	}
	if len(stmts) != len(want) {
		t.Fatalf("got %d statements, want %d:\n%s", len(stmts), len(want), dump(stmts))
	}
	for i := range want {
		if strip(stmts[i].SQL) != want[i] {
			t.Errorf("statement %d = %q, want %q", i, strip(stmts[i].SQL), want[i])
		}
		for _, h := range stmts[i].Hazards {
			if h.Type == safety.IndexDropped {
				t.Errorf("statement %d carries INDEX_DROPPED; a gap-free rebuild never drops first", i)
			}
		}
	}
	for i := 0; i < 3; i++ {
		if stmts[i].Transactional {
			t.Errorf("statement %d (CONCURRENTLY) must not be transactional", i)
		}
	}
	if !stmts[3].Transactional {
		t.Errorf("the rename must be transactional")
	}
	if !hasHazard(stmts[1], safety.IndexBuild) {
		t.Errorf("the concurrent build must carry INDEX_BUILD")
	}
}

// TestRenderReplaceIndex_Offline: without Online (or on a partitioned table,
// where CONCURRENTLY is unavailable) the rebuild is a drop + create inside the
// transaction, both marked as a rebuild.
func TestRenderReplaceIndex_Offline(t *testing.T) {
	for name, op := range map[string]diff.Op{"offline": replaceOp(""), "partitioned": replaceOp("RANGE (a)")} {
		online := name == "partitioned"
		stmts := renderReplaceIndex(op, Options{Online: online})
		if len(stmts) != 3 { // drop, create, comment
			t.Fatalf("%s: got %d statements:\n%s", name, len(stmts), dump(stmts))
		}
		// IF EXISTS: a column drop earlier in the plan may already have taken
		// the old index with it.
		if strip(stmts[0].SQL) != "DROP INDEX IF EXISTS public.t_a;" || !stmts[0].Transactional || !strings.Contains(stmts[0].Note, "rebuilt") {
			t.Errorf("%s: drop = %+v", name, stmts[0])
		}
		if !hasHazard(stmts[0], safety.IndexDropped) {
			t.Errorf("%s: an offline rebuild still removes the index for the transaction's duration", name)
		}
		if !strings.HasPrefix(stmts[1].SQL, "CREATE INDEX t_a ON ") || !stmts[1].Transactional || !strings.Contains(stmts[1].Note, "rebuilt") {
			t.Errorf("%s: create = %+v", name, stmts[1])
		}
	}
}

// TestRenderIndex_Partitioned: CREATE/DROP INDEX CONCURRENTLY are not
// supported on a partitioned table, so online mode falls back to the
// transactional forms there and says so; a plain drop is never IF EXISTS.
func TestRenderIndex_Partitioned(t *testing.T) {
	op := replaceOp("RANGE (a)")
	create := renderCreateIndex(op, Options{Online: true}, false)
	if len(create) != 2 || strings.Contains(create[0].SQL, "CONCURRENTLY") || !create[0].Transactional || !strings.Contains(create[0].Note, "partitioned") {
		t.Errorf("partitioned create online = %+v", create)
	}
	drop := op
	drop.Index = op.PrevIndex
	dropped := renderDropIndex(drop, Options{Online: true}, false)
	if len(dropped) != 1 || strip(dropped[0].SQL) != "DROP INDEX public.t_a;" || !dropped[0].Transactional || !strings.Contains(dropped[0].Note, "partitioned") {
		t.Errorf("partitioned drop online = %+v", dropped)
	}
	plain := renderDropIndex(drop, Options{Online: true}, false)
	if strings.Contains(plain[0].SQL, "IF EXISTS") {
		t.Errorf("a plain drop must not be IF EXISTS: %q", plain[0].SQL)
	}
	if got := strip(renderDropIndex(replaceOp(""), Options{Online: true}, false)[0].SQL); got != "DROP INDEX CONCURRENTLY public.t_a;" {
		t.Errorf("ordinary table online drop = %q", got)
	}
}

func TestConcurrentIndexDef(t *testing.T) {
	cases := map[string]string{
		"CREATE INDEX t_a ON public.t USING btree (a)":        "CREATE INDEX CONCURRENTLY t_a ON public.t USING btree (a)",
		"CREATE UNIQUE INDEX t_u ON public.t USING btree (a)": "CREATE UNIQUE INDEX CONCURRENTLY t_u ON public.t USING btree (a)",
		`CREATE INDEX "My Index" ON public.t USING btree (a)`: `CREATE INDEX CONCURRENTLY "My Index" ON public.t USING btree (a)`,
		"SELECT 1": "SELECT 1",
	}
	for def, want := range cases {
		if got := concurrentIndexDef(def); got != want {
			t.Errorf("concurrentIndexDef(%q) = %q, want %q", def, got, want)
		}
	}
}

func hasHazard(s Statement, typ safety.HazardType) bool {
	for _, h := range s.Hazards {
		if h.Type == typ {
			return true
		}
	}
	return false
}

func dump(stmts []Statement) string {
	var b strings.Builder
	for _, s := range stmts {
		b.WriteString(s.SQL + "\n")
	}
	return b.String()
}
