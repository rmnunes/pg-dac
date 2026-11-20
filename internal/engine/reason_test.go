package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/model"
)

// TestObjectChanges_RecreatedDependentCarriesReason: a view whose definition
// does not change but which must be dropped and recreated because a column it
// reads changes type shows up as a change with identical definitions — and
// says why, so the GUI can explain it instead of showing two equal texts.
func TestObjectChanges_RecreatedDependentCarriesReason(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, cleanup := devdb.WithShared(ctx)
	defer cleanup()

	from, _, err := build.FromSQL(ctx, `CREATE TABLE t(a int); CREATE VIEW v AS SELECT a FROM t;`, build.Options{})
	if err != nil {
		t.Fatalf("build from: %v", err)
	}
	to, _, err := build.FromSQL(ctx, `CREATE TABLE t(a bigint); CREATE VIEW v AS SELECT a FROM t;`, build.Options{})
	if err != nil {
		t.Fatalf("build to: %v", err)
	}
	res, err := engine.Compare(from, to, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	changes, err := res.ObjectChanges()
	if err != nil {
		t.Fatal(err)
	}
	key := model.ObjRef{Kind: model.KindView, Schema: "public", Name: "v"}.Key()
	for _, c := range changes {
		if c.ID != key {
			continue
		}
		if c.ChangeKind != "alter" || c.OldDefinition == "" || c.OldDefinition != c.NewDefinition {
			t.Errorf("view: kind=%q old=%q new=%q", c.ChangeKind, c.OldDefinition, c.NewDefinition)
		}
		if !strings.Contains(c.Reason, "recreated because it depends on column") || !strings.Contains(c.Reason, "whose type changes from integer to bigint") {
			t.Errorf("view reason = %q, want the dependent-recreate explanation", c.Reason)
		}
		return
	}
	t.Fatalf("no change for view v in %s", res.Script(true))
}
