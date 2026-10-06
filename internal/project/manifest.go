// Package project defines the pg-dac declarative project: a directory of .sql
// files described by a pgdac.yaml manifest. The manifest gives a project its
// identity (name/version/targetMajor), schema scope, and — via an ordered list
// of include globs — a deterministic, human-readable deploy order, mirroring the
// numbered-section convention used by ordered "apply.sh" style projects. (Order
// is cosmetic for correctness — the build materializes everything in a scratch
// database that resolves dependencies — but it keeps authoring and apply-error
// messages predictable.)
package project

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// ManifestName is the well-known manifest filename at a project's root.
const ManifestName = "pgdac.yaml"

// Manifest is the parsed pgdac.yaml. Its name/version/targetMajor fields match
// the .pgdac artifact manifest so a project and the artifact built from it stay
// consistent.
type Manifest struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	TargetMajor int      `yaml:"targetMajor"`
	Schemas     []string `yaml:"schemas,omitempty"`
	// Include is the ordered list of globs that defines deploy order.
	Include []string `yaml:"include"`
	// PreDeploy / PostDeploy run before / after the declarative body (seed data,
	// grants, REFRESH MATERIALIZED VIEW, ...).
	PreDeploy  []string `yaml:"preDeploy,omitempty"`
	PostDeploy []string `yaml:"postDeploy,omitempty"`
	// Exclude removes paths from the resolved set.
	Exclude []string `yaml:"exclude,omitempty"`
	// DropPolicy maps to the engine's drop options: none|safe|all.
	DropPolicy string `yaml:"dropPolicy,omitempty"`
}

// DefaultSections are the numbered section folders a new project is scaffolded
// with, ordered to match the engine's dependency bands: functions precede tables
// (a table default/trigger may use a function), and triggers/policies/views come
// after tables.
var DefaultSections = []string{
	"01-extensions", "02-schemas", "03-types", "04-functions", "05-sequences",
	"06-tables", "07-foreign-keys", "08-triggers", "09-policies", "10-views",
}

// Default returns a starter manifest for a project named name.
func Default(name string) *Manifest {
	include := make([]string, len(DefaultSections))
	for i, s := range DefaultSections {
		include[i] = s + "/**/*.sql"
	}
	return &Manifest{
		Name:        name,
		Version:     "0.1.0",
		TargetMajor: 16,
		Include:     include,
		PreDeploy:   []string{"pre-deploy/**/*.sql"},
		PostDeploy:  []string{"post-deploy/**/*.sql"},
		DropPolicy:  "none",
	}
}

// Exists reports whether dir contains a pgdac.yaml.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ManifestName))
	return err == nil
}

// Load reads and parses dir/pgdac.yaml.
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	if len(m.Include) == 0 {
		return nil, fmt.Errorf("%s: 'include' must list at least one glob", ManifestName)
	}
	return &m, nil
}

// Save writes the manifest to dir/pgdac.yaml.
func (m *Manifest) Save(dir string) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), data, 0o644)
}

// IncludeFiles returns the declarative schema-body files (the Include globs
// only), in order. This is what `build` and a comparison materialize into the
// scratch database — pre/post-deploy scripts are deployment-time (seed data,
// grants) and are not part of the schema model.
func (m *Manifest) IncludeFiles(dir string) ([]string, error) {
	return m.resolveGlobs(dir, m.Include)
}

// AllFiles returns every project file in full deploy order: preDeploy, then the
// include globs, then postDeploy.
func (m *Manifest) AllFiles(dir string) ([]string, error) {
	globs := append(append(append([]string{}, m.PreDeploy...), m.Include...), m.PostDeploy...)
	return m.resolveGlobs(dir, globs)
}

// resolveGlobs expands an ordered glob list relative to dir, removing excludes
// and de-duplicating (first match wins); matches within a glob are sorted.
func (m *Manifest) resolveGlobs(dir string, globs []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, glob := range globs {
		matches, err := doublestar.FilepathGlob(filepath.Join(dir, filepath.FromSlash(glob)))
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", glob, err)
		}
		sort.Strings(matches)
		for _, mPath := range matches {
			info, err := os.Stat(mPath)
			if err != nil || info.IsDir() {
				continue
			}
			rel, _ := filepath.Rel(dir, mPath)
			relSlash := filepath.ToSlash(rel)
			if seen[mPath] || isExcluded(relSlash, m.Exclude) {
				continue
			}
			seen[mPath] = true
			out = append(out, mPath)
		}
	}
	return out, nil
}

func isExcluded(relSlash string, excludes []string) bool {
	for _, ex := range excludes {
		if ok, _ := doublestar.Match(ex, relSlash); ok {
			return true
		}
	}
	return false
}
