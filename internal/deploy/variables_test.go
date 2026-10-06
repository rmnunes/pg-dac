package deploy_test

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/project"
)

// $(name) placeholders in deploy scripts resolve from the project's defaults
// overridden by the command line; an unresolved one is an error that names it.
func TestSubstituteVariables(t *testing.T) {
	vars := deploy.MergeVariables(map[string]string{"env": "dev", "retention": "30"}, map[string]string{"env": "prod"})
	got, err := deploy.Substitute(`SELECT '$(env)', $(retention), '$(env)'; -- $$ not a var $(1x) $( ) $(x`, vars)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT 'prod', 30, 'prod'; -- $$ not a var $(1x) $( ) $(x`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	_, err = deploy.Substitute(`GRANT CONNECT ON DATABASE x TO $(reader); SELECT $(missing), $(reader)`, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "missing, reader") || !strings.Contains(err.Error(), "--var") {
		t.Fatalf("undefined variables must be reported once each with the fix: %v", err)
	}
}

// compare.excludeObjectTypes is validated and mapped onto the engine options;
// variables load from the manifest.
func TestManifestCompareAndVariables(t *testing.T) {
	m, err := project.Parse([]byte("name: x\nversion: 1\ntargetMajor: 16\ninclude: [schema/**/*.sql]\ncompare:\n  excludeObjectTypes: [trigger, privilege]\nvariables:\n  env: dev\n"))
	if err != nil {
		t.Fatal(err)
	}
	opts := engine.DefaultOptions()
	deploy.ApplyCompareSettings(&opts, m.Compare)
	if len(opts.ExcludeObjectTypes) != 2 || opts.ExcludeObjectTypes[0] != "trigger" {
		t.Errorf("exclude types not applied: %v", opts.ExcludeObjectTypes)
	}
	if m.Variables["env"] != "dev" {
		t.Errorf("variables not loaded: %v", m.Variables)
	}
	if _, err := project.Parse([]byte("name: x\ninclude: [a]\ncompare:\n  excludeObjectTypes: [tables]\n")); err == nil || !strings.Contains(err.Error(), "tables") {
		t.Errorf("an unknown object type must be rejected: %v", err)
	}
}
