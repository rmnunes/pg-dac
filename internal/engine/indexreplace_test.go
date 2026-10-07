package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/safety"
	"github.com/rmnunes/pg-dac/internal/validate"
)

// TestIndexReplaceIsGapFree: an index whose definition changed is rebuilt the
// way REINDEX CONCURRENTLY works — built under a temporary name while the
// current index serves queries, then swapped in — so no statement leaves the
// table without it and the plan carries no INDEX_DROPPED hazard. Offline the
// rebuild is a drop + create inside the transaction. Both converge on a real
// server.
func TestIndexReplaceIsGapFree(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	from, _, err := build.FromSQL(ctx, `CREATE TABLE t(a int, b int); CREATE INDEX t_a ON t(a); COMMENT ON INDEX t_a IS 'by a';`, build.Options{})
	if err != nil {
		t.Fatalf("build from: %v", err)
	}
	to, _, err := build.FromSQL(ctx, `CREATE TABLE t(a int, b int); CREATE INDEX t_a ON t(a) WHERE b IS NOT NULL; COMMENT ON INDEX t_a IS 'by a';`, build.Options{})
	if err != nil {
		t.Fatalf("build to: %v", err)
	}
	strip := func(s string) string { return strings.ReplaceAll(s, `"`, "") }

	replaceStatements := func(opts engine.Options) []string {
		t.Helper()
		res, err := engine.Compare(from, to, opts)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		var out []string
		var replaces int
		for _, g := range res.Groups {
			if g.Op.Type != diff.OpReplaceIndex {
				continue
			}
			replaces++
			for _, s := range g.Statements {
				out = append(out, strip(s.SQL))
				for _, h := range s.Hazards {
					if h.Type == safety.IndexDropped && opts.Online {
						t.Errorf("online rebuild raised INDEX_DROPPED on %q", s.SQL)
					}
				}
			}
		}
		if replaces != 1 {
			t.Fatalf("want one replace op, got %d in:\n%s", replaces, res.Script(true))
		}
		vr, err := validate.Plan(ctx, from, to, opts)
		if err != nil {
			t.Fatalf("migration failed to apply (online=%v): %v\n%s", opts.Online, err, res.Script(true))
		}
		if !vr.Converged {
			t.Fatalf("migration did not converge (online=%v); residual:\n%s", opts.Online, vr.Residual.Script(true))
		}
		return out
	}

	online := replaceStatements(engine.DefaultOptions())
	wantPrefixes := []string{
		"DROP INDEX CONCURRENTLY IF EXISTS public.t_a_pgdac_new;",
		"CREATE INDEX CONCURRENTLY t_a_pgdac_new ON public.t ",
		"DROP INDEX CONCURRENTLY IF EXISTS public.t_a;",
		"ALTER INDEX public.t_a_pgdac_new RENAME TO t_a;",
		"COMMENT ON INDEX public.t_a IS 'by a';",
	}
	if len(online) != len(wantPrefixes) {
		t.Fatalf("online: got %d statements, want %d:\n%s", len(online), len(wantPrefixes), strings.Join(online, "\n"))
	}
	for i, p := range wantPrefixes {
		if !strings.HasPrefix(online[i], p) {
			t.Errorf("online statement %d = %q, want prefix %q", i, online[i], p)
		}
	}

	opts := engine.DefaultOptions()
	opts.Online = false
	offline := replaceStatements(opts)
	if len(offline) != 3 || !strings.HasPrefix(offline[0], "DROP INDEX public.t_a;") || !strings.HasPrefix(offline[1], "CREATE INDEX t_a ON public.t ") {
		t.Errorf("offline: want drop + create + comment, got:\n%s", strings.Join(offline, "\n"))
	}
}
