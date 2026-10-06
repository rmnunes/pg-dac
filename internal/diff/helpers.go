package diff

import (
	"sort"

	"github.com/rodrigonunes/pg-dac/internal/model"
)

func extRef(e *model.Extension) model.ObjRef {
	return model.ObjRef{Kind: model.KindExtension, Name: e.Name}
}

func schemaRef(s *model.Schema) model.ObjRef {
	return model.ObjRef{Kind: model.KindSchema, Name: s.Name}
}

func indexSchemas(d *model.Database) map[string]*model.Schema {
	m := make(map[string]*model.Schema, len(d.Schemas))
	for _, s := range d.Schemas {
		m[s.Name] = s
	}
	return m
}

func indexEnums(s *model.Schema) map[string]*model.EnumType {
	m := map[string]*model.EnumType{}
	if s == nil {
		return m
	}
	for _, e := range s.Enums {
		m[e.Name] = e
	}
	return m
}

func indexCollations(s *model.Schema) map[string]*model.Collation {
	m := map[string]*model.Collation{}
	if s == nil {
		return m
	}
	for _, c := range s.Collations {
		m[c.Name] = c
	}
	return m
}

func indexSequences(s *model.Schema) map[string]*model.Sequence {
	m := map[string]*model.Sequence{}
	if s == nil {
		return m
	}
	for _, seq := range s.Sequences {
		m[seq.Name] = seq
	}
	return m
}

func indexTables(s *model.Schema) map[string]*model.Table {
	m := map[string]*model.Table{}
	if s == nil {
		return m
	}
	for _, t := range s.Tables {
		m[t.Name] = t
	}
	return m
}

func indexViews(s *model.Schema) map[string]*model.View {
	m := map[string]*model.View{}
	if s == nil {
		return m
	}
	for _, v := range s.Views {
		m[v.Name] = v
	}
	return m
}

func indexMatViews(s *model.Schema) map[string]*model.MaterializedView {
	m := map[string]*model.MaterializedView{}
	if s == nil {
		return m
	}
	for _, mv := range s.MatViews {
		m[mv.Name] = mv
	}
	return m
}

func indexFunctions(s *model.Schema) map[string]*model.Function {
	m := map[string]*model.Function{}
	if s == nil {
		return m
	}
	for _, f := range s.Functions {
		m[f.Signature()] = f
	}
	return m
}

func indexTriggers(t *model.Table) map[string]*model.Trigger {
	m := make(map[string]*model.Trigger, len(t.Triggers))
	for _, tr := range t.Triggers {
		m[tr.Name] = tr
	}
	return m
}

func indexPolicies(t *model.Table) map[string]*model.Policy {
	m := make(map[string]*model.Policy, len(t.Policies))
	for _, p := range t.Policies {
		m[p.Name] = p
	}
	return m
}

func indexColumns(t *model.Table) map[string]*model.Column {
	m := make(map[string]*model.Column, len(t.Columns))
	for _, c := range t.Columns {
		m[c.Name] = c
	}
	return m
}

func indexConstraints(t *model.Table) map[string]*model.Constraint {
	m := make(map[string]*model.Constraint, len(t.Constraints))
	for _, c := range t.Constraints {
		m[c.Name] = c
	}
	return m
}

func indexIndexes(t *model.Table) map[string]*model.Index {
	m := make(map[string]*model.Index, len(t.Indexes))
	for _, i := range t.Indexes {
		m[i.Name] = i
	}
	return m
}

// unionKeys returns the sorted union of the keys of two schema maps, so that
// per-schema diffing is deterministic.
func unionKeys(a, b map[string]*model.Schema) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// appendedValues returns the labels present in cur that are not in prev,
// preserving cur's order. Used to detect enum values that can be added online.
func appendedValues(prev, cur []string) []string {
	have := map[string]struct{}{}
	for _, v := range prev {
		have[v] = struct{}{}
	}
	var added []string
	for _, v := range cur {
		if _, ok := have[v]; !ok {
			added = append(added, v)
		}
	}
	return added
}

func policyChanged(a, b *model.Policy) bool {
	if a.Permissive != b.Permissive || a.Command != b.Command ||
		a.Using != b.Using || a.WithCheck != b.WithCheck {
		return true
	}
	if len(a.Roles) != len(b.Roles) {
		return true
	}
	for i := range a.Roles {
		if a.Roles[i] != b.Roles[i] {
			return true
		}
	}
	return false
}

func sequenceChanged(a, b *model.Sequence) bool {
	return a.DataType != b.DataType ||
		a.Increment != b.Increment ||
		a.MinValue != b.MinValue ||
		a.MaxValue != b.MaxValue ||
		a.Start != b.Start ||
		a.Cache != b.Cache ||
		a.Cycle != b.Cycle
}
