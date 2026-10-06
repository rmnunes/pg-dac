// Package deploy carries the deployment-time parts of a project that are not
// schema: pre-deploy and post-deploy scripts (seed data, grants, REFRESH
// MATERIALIZED VIEW, ...) and the manifest's drop policy. They travel inside a
// .pgdac next to the model, so an artifact is a complete deployment unit the
// way a DACPAC is, and `publish` runs them around the generated migration.
package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/refactor"
)

// Script is one deployment script: a display name and its SQL.
type Script struct {
	Name string
	SQL  string
}

// Plan is what a desired-state source contributes to a deployment besides the
// schema model.
type Plan struct {
	Pre, Post  []Script
	DropPolicy string // "" | none | safe | all
	// ManifestYAML is the project's pgdac.yaml text, when the source has one.
	ManifestYAML []byte
	// Refactor is the project's refactorlog (renames to apply once per target).
	Refactor *refactor.Log
	// Compare holds the project's comparison defaults (pgdac.yaml compare:).
	Compare *project.CompareSettings
	// Variables are the project's default values for the $(name) placeholders
	// its deploy scripts use (pgdac.yaml variables:); the command line overrides.
	Variables map[string]string
}

// Empty reports whether the plan carries nothing.
func (p *Plan) Empty() bool {
	return p == nil || (len(p.Pre) == 0 && len(p.Post) == 0 && p.DropPolicy == "" && len(p.ManifestYAML) == 0 &&
		(p.Refactor == nil || len(p.Refactor.Operations) == 0))
}

// ForSource inspects a desired-state spec: a project directory with a
// pgdac.yaml yields its pre/post-deploy files (in manifest order) and drop
// policy; a .pgdac yields the ones embedded at build time; anything else (a
// live database, a bare .sql file) yields an empty plan.
func ForSource(spec string) (*Plan, error) {
	spec = strings.TrimPrefix(spec, "file://")
	if strings.HasSuffix(spec, ".pgdac") {
		o, err := artifact.Open(spec)
		if err != nil {
			return nil, err
		}
		return fromArtifact(o), nil
	}
	fi, err := os.Stat(spec)
	if err != nil || !fi.IsDir() || !project.Exists(spec) {
		return &Plan{}, nil
	}
	m, err := project.Load(spec)
	if err != nil {
		return nil, err
	}
	plan := &Plan{DropPolicy: m.DropPolicy, Compare: m.Compare, Variables: m.Variables}
	if plan.ManifestYAML, err = os.ReadFile(filepath.Join(spec, project.ManifestName)); err != nil {
		return nil, err
	}
	if plan.Refactor, err = refactor.Load(spec); err != nil {
		return nil, err
	}
	if plan.Pre, err = readScripts(spec, m.PreDeployFiles); err != nil {
		return nil, err
	}
	if plan.Post, err = readScripts(spec, m.PostDeployFiles); err != nil {
		return nil, err
	}
	return plan, nil
}

func readScripts(dir string, list func(string) ([]string, error)) ([]Script, error) {
	files, err := list(dir)
	if err != nil {
		return nil, err
	}
	var out []Script
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		rel, _ := filepath.Rel(dir, f)
		out = append(out, Script{Name: filepath.ToSlash(rel), SQL: string(data)})
	}
	return out, nil
}

func fromArtifact(o *artifact.Opened) *Plan {
	plan := &Plan{}
	if o.Manifest.Deploy != nil {
		plan.DropPolicy = o.Manifest.Deploy.DropPolicy
	}
	plan.ManifestYAML = o.Parts[artifact.PartProject]
	if len(plan.ManifestYAML) > 0 {
		if m, err := project.Parse(plan.ManifestYAML); err == nil {
			plan.Compare = m.Compare
			plan.Variables = m.Variables
		}
	}
	if data, ok := o.Parts[artifact.PartRefactorLog]; ok {
		if log, err := refactor.Parse(data); err == nil {
			plan.Refactor = log
		}
	}
	for _, name := range partsUnder(o, artifact.PartPreDeployDir) {
		plan.Pre = append(plan.Pre, Script{Name: name, SQL: string(o.Parts[name])})
	}
	for _, name := range partsUnder(o, artifact.PartPostDeployDir) {
		plan.Post = append(plan.Post, Script{Name: name, SQL: string(o.Parts[name])})
	}
	return plan
}

// partsUnder lists an artifact's parts below a directory prefix in name order
// (names carry a zero-padded sequence, so this is the execution order).
func partsUnder(o *artifact.Opened, prefix string) []string {
	var names []string
	for name := range o.Parts {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ArtifactParts renders the plan as extra artifact parts (manifest text plus
// numbered pre/post scripts) and the manifest's deploy block.
func (p *Plan) ArtifactParts() ([]artifact.ExtraPart, *artifact.DeployInfo) {
	if p.Empty() {
		return nil, nil
	}
	var parts []artifact.ExtraPart
	info := &artifact.DeployInfo{DropPolicy: p.DropPolicy}
	if len(p.ManifestYAML) > 0 {
		parts = append(parts, artifact.ExtraPart{Name: artifact.PartProject, Data: p.ManifestYAML})
	}
	if p.Refactor != nil && len(p.Refactor.Operations) > 0 {
		if data, err := p.Refactor.Bytes(); err == nil {
			parts = append(parts, artifact.ExtraPart{Name: artifact.PartRefactorLog, Data: data})
		}
	}
	for i, s := range p.Pre {
		name := fmt.Sprintf("%s%04d_%s", artifact.PartPreDeployDir, i+1, filepath.Base(s.Name))
		parts = append(parts, artifact.ExtraPart{Name: name, Data: []byte(s.SQL)})
		info.PreDeploy = append(info.PreDeploy, name)
	}
	for i, s := range p.Post {
		name := fmt.Sprintf("%s%04d_%s", artifact.PartPostDeployDir, i+1, filepath.Base(s.Name))
		parts = append(parts, artifact.ExtraPart{Name: name, Data: []byte(s.SQL)})
		info.PostDeploy = append(info.PostDeploy, name)
	}
	return parts, info
}

// Substitute replaces every $(name) placeholder in a deploy script with its
// value (SQLCMD-style variables: environment names, role names, retention
// values). A placeholder without a value is an error that names it, so a
// script never runs with a literal "$(name)" in it. Schema files are never
// substituted: a model must not depend on the environment.
func Substitute(sql string, vars map[string]string) (string, error) {
	var out strings.Builder
	var missing []string
	i := 0
	for i < len(sql) {
		if sql[i] == '$' && i+1 < len(sql) && sql[i+1] == '(' {
			j := i + 2
			// A variable name starts with a letter or underscore; "$(1x)" is text.
			if j < len(sql) && (sql[j] == '_' || (sql[j] >= 'a' && sql[j] <= 'z') || (sql[j] >= 'A' && sql[j] <= 'Z')) {
				for j < len(sql) && (sql[j] == '_' || (sql[j] >= 'a' && sql[j] <= 'z') || (sql[j] >= 'A' && sql[j] <= 'Z') || (sql[j] >= '0' && sql[j] <= '9')) {
					j++
				}
			}
			if j > i+2 && j < len(sql) && sql[j] == ')' {
				name := sql[i+2 : j]
				if v, ok := vars[name]; ok {
					out.WriteString(v)
				} else {
					missing = append(missing, name)
					out.WriteString(sql[i : j+1])
				}
				i = j + 1
				continue
			}
		}
		out.WriteByte(sql[i])
		i++
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("undefined variable(s) %s: define them in %s variables: or pass --var name=value", strings.Join(uniq(missing), ", "), project.ManifestName)
	}
	return out.String(), nil
}

func uniq(ss []string) []string {
	var out []string
	for i, s := range ss {
		if i == 0 || s != ss[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// MergeVariables layers overrides (the command line) over a project's defaults.
func MergeVariables(defaults, overrides map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ApplyCompareSettings maps a project's compare block onto the engine options;
// fields the project leaves unset keep the options' current values.
func ApplyCompareSettings(opts *engine.Options, cs *project.CompareSettings) {
	if cs == nil {
		return
	}
	if cs.Privileges != nil {
		opts.IgnorePrivileges = !*cs.Privileges
	}
	if cs.ColumnOrder != "" {
		opts.ColumnOrder = cs.ColumnOrder
	}
	if cs.IgnoreWhitespace != nil {
		opts.CompareWhitespace = !*cs.IgnoreWhitespace
	}
	if cs.IgnoreComments != nil {
		opts.IgnoreComments = *cs.IgnoreComments
	}
	if len(cs.ExcludeObjectTypes) > 0 {
		opts.ExcludeObjectTypes = append([]string{}, cs.ExcludeObjectTypes...)
	}
}

// ApplyDropPolicy maps a manifest drop policy onto the engine options:
//
//	none  drop nothing that is missing from the source
//	safe  drop only constraints and indexes (cheap to recreate) — the default
//	all   drop every object missing from the source
//
// An unknown or empty policy leaves the options untouched.
func ApplyDropPolicy(opts *engine.Options, policy string) {
	switch strings.ToLower(policy) {
	case "none":
		opts.DropObjectsNotInSource = false
		opts.DropConstraintsNotInSource = false
		opts.DropIndexesNotInSource = false
	case "safe":
		opts.DropObjectsNotInSource = false
		opts.DropConstraintsNotInSource = true
		opts.DropIndexesNotInSource = true
	case "all":
		opts.DropObjectsNotInSource = true
		opts.DropConstraintsNotInSource = true
		opts.DropIndexesNotInSource = true
	}
}

// Run executes scripts in order, each as one multi-statement script in its own
// implicit transaction (simple query protocol), after substituting its $(name)
// variables. Scripts are plain SQL — psql meta-commands such as \i cannot run
// server-side.
func Run(ctx context.Context, conn *pgx.Conn, scripts []Script, vars map[string]string, onScript func(name string)) error {
	for _, s := range scripts {
		if onScript != nil {
			onScript(s.Name)
		}
		sql, err := Substitute(s.SQL, vars)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		if err := execute.ApplyScript(ctx, conn, sql); err != nil {
			return fmt.Errorf("deploy script %s: %w", s.Name, err)
		}
	}
	return nil
}
