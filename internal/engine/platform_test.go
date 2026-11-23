package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

func withExtension(version int, extVersion string) *model.Database {
	return &model.Database{
		TargetVersion: version,
		Schemas:       []*model.Schema{{Name: "public"}},
		Extensions:    []*model.Extension{{Name: "btree_gist", Schema: "public", Version: extVersion}},
	}
}

// A desired state normalized for a newer major than the target runs is
// refused, as SqlPackage refuses a project whose target platform is newer
// than the server: the newer server's extension version would otherwise be
// planned as an upgrade the target cannot perform.
func TestCompareRefusesNewerPlatform(t *testing.T) {
	from, to := withExtension(160004, "1.7"), withExtension(180001, "1.8")

	_, err := Compare(from, to, DefaultOptions())
	if err == nil {
		t.Fatal("a PostgreSQL 18 desired state against a 16 target was compared")
	}
	for _, want := range []string{"PostgreSQL 18", "PostgreSQL 16", "--target-major 16", "--allow-incompatible-platform"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	opts := DefaultOptions()
	opts.AllowIncompatiblePlatform = true
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatalf("with AllowIncompatiblePlatform: %v", err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "PostgreSQL 18") {
		t.Errorf("the mismatch is not reported as a warning: %v", res.Warnings)
	}
}

// The same major, an older desired state, or a model without a version are
// not a platform mismatch.
func TestComparePlatformAccepted(t *testing.T) {
	cases := map[string][2]int{
		"same major":      {160004, 160010},
		"older desired":   {180001, 160004},
		"unknown target":  {0, 180001},
		"unknown desired": {160004, 0},
	}
	for name, v := range cases {
		if _, err := Compare(withExtension(v[0], "1.7"), withExtension(v[1], "1.7"), DefaultOptions()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Statements run on the target, so version-gated forms follow the target's
// major, not the desired state's: a PostgreSQL 17 target gets SET EXPRESSION
// even for a desired state normalized on 16, and a 16 target never gets it.
func TestStrategiesFollowTheTargetMajor(t *testing.T) {
	table := func(version int, expr string) *model.Database {
		return &model.Database{TargetVersion: version, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{{
			Schema: "app", Name: "t",
			Columns: []*model.Column{
				{Name: "a", Type: "integer"},
				{Name: "b", Type: "integer", Generated: "stored", GeneratedExpr: expr},
			},
		}}}}}
	}
	script := func(from, to *model.Database, opts Options) string {
		t.Helper()
		res, err := Compare(from, to, opts)
		if err != nil {
			t.Fatal(err)
		}
		return res.Script(true)
	}

	got := script(table(170002, "(a * 2)"), table(160004, "(a * 3)"), DefaultOptions())
	if !strings.Contains(got, "SET EXPRESSION") {
		t.Errorf("a PostgreSQL 17 target should get SET EXPRESSION:\n%s", got)
	}

	opts := DefaultOptions()
	opts.AllowIncompatiblePlatform = true
	got = script(table(160004, "(a * 2)"), table(170002, "(a * 3)"), opts)
	if strings.Contains(got, "SET EXPRESSION") {
		t.Errorf("a PostgreSQL 16 target must not get SET EXPRESSION (17+):\n%s", got)
	}
}
