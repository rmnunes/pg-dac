package deploy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/project"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForSourceProject(t *testing.T) {
	dir := t.TempDir()
	m := project.Default("x")
	m.DropPolicy = "all"
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "pre-deploy", "02_b.sql"), "SELECT 2;")
	writeFile(t, filepath.Join(dir, "pre-deploy", "01_a.sql"), "SELECT 1;")
	writeFile(t, filepath.Join(dir, "post-deploy", "seed.sql"), "SELECT 'seed';")
	writeFile(t, filepath.Join(dir, "06-tables", "t.sql"), "CREATE TABLE t(a int);")

	plan, err := deploy.ForSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DropPolicy != "all" || len(plan.ManifestYAML) == 0 {
		t.Errorf("policy %q, manifest %d bytes", plan.DropPolicy, len(plan.ManifestYAML))
	}
	if len(plan.Pre) != 2 || plan.Pre[0].Name != "pre-deploy/01_a.sql" || plan.Pre[1].SQL != "SELECT 2;" {
		t.Errorf("pre scripts: %+v", plan.Pre)
	}
	if len(plan.Post) != 1 || plan.Post[0].Name != "post-deploy/seed.sql" {
		t.Errorf("post scripts: %+v", plan.Post)
	}

	parts, info := plan.ArtifactParts()
	if info == nil || info.DropPolicy != "all" || len(info.PreDeploy) != 2 || len(info.PostDeploy) != 1 {
		t.Fatalf("deploy info: %+v", info)
	}
	if info.PreDeploy[0] != artifact.PartPreDeployDir+"0001_01_a.sql" || info.PostDeploy[0] != artifact.PartPostDeployDir+"0001_seed.sql" {
		t.Errorf("part names: %v %v", info.PreDeploy, info.PostDeploy)
	}
	names := map[string]bool{}
	for _, p := range parts {
		names[p.Name] = true
	}
	if !names[artifact.PartProject] || len(parts) != 4 {
		t.Errorf("parts: %v", names)
	}
}

func TestForSourceNonProject(t *testing.T) {
	for _, spec := range []string{t.TempDir(), "postgres://u:p@localhost/db", "nowhere.sql"} {
		plan, err := deploy.ForSource(spec)
		if err != nil || !plan.Empty() {
			t.Errorf("%s: expected an empty plan, got %+v (err %v)", spec, plan, err)
		}
	}
}

func TestApplyDropPolicy(t *testing.T) {
	cases := map[string][3]bool{ // objects, constraints, indexes
		"none": {false, false, false},
		"safe": {false, true, true},
		"all":  {true, true, true},
	}
	for policy, want := range cases {
		opts := engine.DefaultOptions()
		opts.DropObjectsNotInSource = true // must be overridden by the policy
		deploy.ApplyDropPolicy(&opts, policy)
		got := [3]bool{opts.DropObjectsNotInSource, opts.DropConstraintsNotInSource, opts.DropIndexesNotInSource}
		if got != want {
			t.Errorf("%s: got %v want %v", policy, got, want)
		}
	}
	opts := engine.DefaultOptions()
	deploy.ApplyDropPolicy(&opts, "")
	def := engine.DefaultOptions()
	if opts.DropObjectsNotInSource != def.DropObjectsNotInSource || opts.DropConstraintsNotInSource != def.DropConstraintsNotInSource || opts.DropIndexesNotInSource != def.DropIndexesNotInSource {
		t.Error("empty policy must leave the options untouched")
	}
}

// Deploy scripts run as plain multi-statement SQL, in order, each in its own
// implicit transaction.
func TestRunScripts(t *testing.T) {
	if testing.Short() {
		t.Skip("needs embedded postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sc, err := devdb.Acquire(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Release()
	conn, err := pgx.Connect(ctx, sc.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	var ran []string
	err = deploy.Run(ctx, conn, []deploy.Script{
		{Name: "a", SQL: "CREATE TABLE seed(x int); INSERT INTO seed VALUES (1), (2);"},
		{Name: "b", SQL: "INSERT INTO seed SELECT x + 10 FROM seed;"},
	}, func(name string) { ran = append(ran, name) })
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM seed").Scan(&n); err != nil || n != 4 {
		t.Errorf("expected 4 rows, got %d (err %v)", n, err)
	}
	if strings.Join(ran, ",") != "a,b" {
		t.Errorf("ran %v", ran)
	}
	if err := deploy.Run(ctx, conn, []deploy.Script{{Name: "bad", SQL: "SELECT * FROM nowhere;"}}, nil); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("a failing script must report its name, got %v", err)
	}
}
