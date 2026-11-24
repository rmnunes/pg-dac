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
	// Compare holds the project's defaults for what counts as a difference.
	Compare *CompareSettings `yaml:"compare,omitempty"`
	// Roles are roles the project's GRANTs name but do not create (they belong
	// to the environment, like SSDT's SQLCMD-variable users). They are created
	// in the scratch database so the project builds, and a publish refuses a
	// target that lacks one.
	Roles []string `yaml:"roles,omitempty"`
	// Owner is the role that owns the project's objects. The scratch build
	// and a publish create them as this role (SET ROLE), so the default
	// privileges FOR ROLE it apply on both sides; objects the target has under
	// another owner are reassigned (a blocking hazard: the previous owner
	// loses what it held as owner). Like roles:, it belongs to the
	// environment, and the deploying role must be able to SET ROLE to it.
	// Empty: objects are owned by whoever deploys them, and ownership is not
	// compared.
	Owner string `yaml:"owner,omitempty"`
	// Variables are default values for the $(name) placeholders the project's
	// pre/post-deploy scripts use (SQLCMD variables); --var overrides them.
	Variables map[string]string `yaml:"variables,omitempty"`
}

// CompareSettings are the SSDT-style comparison options a project can pin.
// A nil pointer / empty string means pg-dac's default.
type CompareSettings struct {
	// Privileges: compare GRANT/REVOKE (default true).
	Privileges *bool `yaml:"privileges,omitempty"`
	// ColumnOrder: ignore | warn (default) | rebuild — what to do when a table's
	// declared column order is one PostgreSQL cannot reach with ALTERs.
	ColumnOrder string `yaml:"columnOrder,omitempty"`
	// IgnoreWhitespace: routine bodies compare modulo whitespace (default true).
	IgnoreWhitespace *bool `yaml:"ignoreWhitespace,omitempty"`
	// IgnoreComments: a comment-only change in a routine body is not a change
	// (default false).
	IgnoreComments *bool `yaml:"ignoreComments,omitempty"`
	// Ownership: reassign objects the target has under another owner than
	// the project's owner: (default true; SSDT's IgnoreAuthorizer inverted).
	// Off, they keep their owner and pg-dac changes them as the deploying role.
	Ownership *bool `yaml:"ownership,omitempty"`
	// ExcludeObjectTypes leaves whole kinds of object out of the comparison
	// (SSDT's ExcludeObjectTypes): table, view, matview, sequence, function,
	// enum, domain, compositeType, collation, schema, extension, index,
	// constraint, trigger, policy, privilege, comment.
	ExcludeObjectTypes []string `yaml:"excludeObjectTypes,omitempty"`
}

// ColumnOrderPolicies are the accepted values of compare.columnOrder.
var ColumnOrderPolicies = []string{"ignore", "warn", "rebuild"}

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

// AddRoles appends roles to the manifest's roles: list, editing the YAML in
// place so the file's comments and layout survive (an update from a database
// must not reformat a hand-written manifest).
func AddRoles(dir string, roles []string) error {
	path := filepath.Join(dir, ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: unexpected document structure", ManifestName)
	}
	root := doc.Content[0]
	var seq *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "roles" {
			seq = root.Content[i+1]
		}
	}
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "roles"}, seq)
	}
	have := map[string]bool{}
	for _, n := range seq.Content {
		have[n.Value] = true
	}
	for _, r := range roles {
		if !have[r] {
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: r})
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
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
	return Parse(data)
}

// Parse parses and validates manifest text (a pgdac.yaml on disk or the copy an
// artifact carries).
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	if len(m.Include) == 0 {
		return nil, fmt.Errorf("%s: 'include' must list at least one glob", ManifestName)
	}
	if m.Compare != nil && m.Compare.ColumnOrder != "" && !ValidColumnOrder(m.Compare.ColumnOrder) {
		return nil, fmt.Errorf("%s: compare.columnOrder must be one of %v, got %q", ManifestName, ColumnOrderPolicies, m.Compare.ColumnOrder)
	}
	if m.Compare != nil {
		for _, t := range m.Compare.ExcludeObjectTypes {
			if !ValidObjectType(t) {
				return nil, fmt.Errorf("%s: compare.excludeObjectTypes has unknown type %q (valid: %v)", ManifestName, t, ObjectTypes)
			}
		}
	}
	return &m, nil
}

// ObjectTypes are the kinds compare.excludeObjectTypes / --exclude-object-type accept.
var ObjectTypes = []string{"table", "view", "matview", "sequence", "function", "enum", "domain", "compositeType",
	"collation", "schema", "extension", "index", "constraint", "trigger", "policy", "privilege", "defaultPrivilege", "comment"}

// ValidObjectType reports whether t names an excludable object type.
func ValidObjectType(t string) bool {
	for _, v := range ObjectTypes {
		if v == t {
			return true
		}
	}
	return false
}

// ValidColumnOrder reports whether p is an accepted column-order policy.
func ValidColumnOrder(p string) bool {
	for _, v := range ColumnOrderPolicies {
		if v == p {
			return true
		}
	}
	return false
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

// PreDeployFiles / PostDeployFiles return the deployment scripts that run
// before / after the generated migration, in manifest order.
func (m *Manifest) PreDeployFiles(dir string) ([]string, error) {
	return m.resolveGlobs(dir, m.PreDeploy)
}
func (m *Manifest) PostDeployFiles(dir string) ([]string, error) {
	return m.resolveGlobs(dir, m.PostDeploy)
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
