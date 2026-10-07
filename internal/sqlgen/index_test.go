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
		if strip(stmts[0].SQL) != "DROP INDEX public.t_a;" || !stmts[0].Transactional || !strings.Contains(stmts[0].Note, "rebuilt") {
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
