package diff

import (
	"sort"
	"strings"

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

// enumAdditions returns the labels in cur that are missing from prev, each with
// the position to insert it at so the final order matches cur: a label goes
// AFTER the label that precedes it in cur (which exists by the time it is
// added, being either old or just added), or — for the very first label —
// BEFORE the next label that already exists.
func enumAdditions(prev, cur []string) ([]string, []EnumPosition) {
	have := map[string]struct{}{}
	for _, v := range prev {
		have[v] = struct{}{}
	}
	var added []string
	var positions []EnumPosition
	for i, v := range cur {
		if _, ok := have[v]; ok {
			continue
		}
		var p EnumPosition
		switch {
		case i > 0:
			p.After = cur[i-1]
		default:
			for _, next := range cur[1:] {
				if _, ok := have[next]; ok {
					p.Before = next
					break
				}
			}
		}
		added = append(added, v)
		positions = append(positions, p)
	}
	return added, positions
}

// missingValues returns the labels of prev that cur no longer has.
func missingValues(prev, cur []string) []string {
	have := map[string]struct{}{}
	for _, v := range cur {
		have[v] = struct{}{}
	}
	var out []string
	for _, v := range prev {
		if _, ok := have[v]; !ok {
			out = append(out, v)
		}
	}
	return out
}

// sameRelativeOrder reports whether the labels common to prev and cur appear in
// the same order in both.
func sameRelativeOrder(prev, cur []string) bool {
	pos := map[string]int{}
	for i, v := range cur {
		pos[v] = i
	}
	last := -1
	for _, v := range prev {
		i, ok := pos[v]
		if !ok {
			continue
		}
		if i < last {
			return false
		}
		last = i
	}
	return true
}

func quoteList(vals []string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = model.QuoteLiteral(v)
	}
	return strings.Join(parts, ", ")
}

// relOptionsDiff returns the storage parameters to SET (new or changed) and to
// RESET (gone), the latter sorted for deterministic output.
func relOptionsDiff(prev, cur map[string]string) (set map[string]string, reset []string) {
	for k, v := range cur {
		if pv, ok := prev[k]; !ok || pv != v {
			if set == nil {
				set = map[string]string{}
			}
			set[k] = v
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			reset = append(reset, k)
		}
	}
	sort.Strings(reset)
	return set, reset
}

func indexDomains(s *model.Schema) map[string]*model.Domain {
	m := map[string]*model.Domain{}
	if s == nil {
		return m
	}
	for _, d := range s.Domains {
		m[d.Name] = d
	}
	return m
}

func indexComposites(s *model.Schema) map[string]*model.CompositeType {
	m := map[string]*model.CompositeType{}
	if s == nil {
		return m
	}
	for _, c := range s.Composites {
		m[c.Name] = c
	}
	return m
}

func domainsOf(s *model.Schema) []*model.Domain {
	if s == nil {
		return nil
	}
	return s.Domains
}

func compositesOf(s *model.Schema) []*model.CompositeType {
	if s == nil {
		return nil
	}
	return s.Composites
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

// --- nil-safe collection accessors (a side may lack the schema entirely) ---

func enumsOf(s *model.Schema) []*model.EnumType {
	if s == nil {
		return nil
	}
	return s.Enums
}

func collationsOf(s *model.Schema) []*model.Collation {
	if s == nil {
		return nil
	}
	return s.Collations
}

func sequencesOf(s *model.Schema) []*model.Sequence {
	if s == nil {
		return nil
	}
	return s.Sequences
}

func functionsOf(s *model.Schema) []*model.Function {
	if s == nil {
		return nil
	}
	return s.Functions
}

func tablesOf(s *model.Schema) []*model.Table {
	if s == nil {
		return nil
	}
	return s.Tables
}

func viewsOf(s *model.Schema) []*model.View {
	if s == nil {
		return nil
	}
	return s.Views
}

func matViewsOf(s *model.Schema) []*model.MaterializedView {
	if s == nil {
		return nil
	}
	return s.MatViews
}

// viewReplaceCompatible reports whether cur can replace prev with CREATE OR
// REPLACE VIEW: PostgreSQL allows that only when the existing columns keep their
// names and types in the same positions and new columns are appended at the
// end. Unknown column lists (a model from an older artifact) are assumed
// compatible, which is the pre-WP2 behavior.
func viewReplaceCompatible(prev, cur *model.View) bool {
	if len(prev.Columns) == 0 || len(cur.Columns) == 0 {
		return true
	}
	if len(cur.Columns) < len(prev.Columns) {
		return false
	}
	for i, pc := range prev.Columns {
		if cur.Columns[i] != pc {
			return false
		}
	}
	return true
}

// functionRecreateReason returns why a changed function cannot be applied with
// CREATE OR REPLACE (empty when it can): a different result type, a function
// turned into a procedure or back, or a change to OUT parameters (which alter
// the result row type). Unknown metadata (older artifacts) is treated as
// compatible.
func functionRecreateReason(prev, cur *model.Function) string {
	switch {
	case prev.Kind != cur.Kind:
		return "routine kind changed (" + prev.Kind + " → " + cur.Kind + "); CREATE OR REPLACE cannot change it, so the routine is recreated"
	case prev.ReturnType != "" && cur.ReturnType != "" && prev.ReturnType != cur.ReturnType:
		return "return type changed (" + prev.ReturnType + " → " + cur.ReturnType + "); CREATE OR REPLACE cannot change it, so the function is recreated"
	case prev.Arguments != "" && cur.Arguments != "" && prev.Arguments != cur.Arguments &&
		(hasOutParams(prev.Arguments) || hasOutParams(cur.Arguments)):
		return "OUT parameters changed; CREATE OR REPLACE cannot change the result row type, so the function is recreated"
	}
	return ""
}

func hasOutParams(args string) bool {
	return strings.Contains(args, "OUT ")
}

// sameOwner compares two optional sequence owners.
func sameOwner(a, b *model.ColumnRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
