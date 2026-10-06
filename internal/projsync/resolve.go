package projsync

import (
	"strings"

	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/sqltext"
)

// unitKey is the key statements and changes are grouped under: an object's
// own key, except routines, whose overloads share one unit per name because a
// statement's argument list cannot be matched to the catalog's identity form.
func unitKey(ref model.ObjRef) string {
	if ref.Kind == model.KindFunction {
		name := ref.Name
		if p := strings.IndexByte(name, '('); p >= 0 {
			name = name[:p]
		}
		return "routines:" + model.QualifiedName(ref.Schema, name)
	}
	return ref.Key()
}

// refOfUnit recovers an object reference from a non-routine unit key.
func refOfUnit(u string) model.ObjRef {
	kind, rest, _ := strings.Cut(u, ":")
	parts := splitQualified(rest)
	switch len(parts) {
	case 1:
		return model.ObjRef{Kind: model.ObjKind(kind), Name: parts[0]}
	default:
		return model.ObjRef{Kind: model.ObjKind(kind), Schema: parts[0], Name: strings.Join(parts[1:], ".")}
	}
}

func describeUnit(u string) string {
	if strings.HasPrefix(u, "routines:") {
		return "routine " + strings.TrimPrefix(u, "routines:")
	}
	kind, rest, _ := strings.Cut(u, ":")
	return kind + " " + rest
}

// splitQualified splits a quoted dotted name produced by model.QualifiedName.
func splitQualified(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			if inQuote && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte('"')
				i++
				continue
			}
			inQuote = !inQuote
		case c == '.' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(parts, cur.String())
}

// resolver maps statement heads to owner units using the project's model.
type resolver struct {
	relKind  map[string]model.ObjKind // "schema\x1fname" -> table | view | matview
	typeKind map[string]model.ObjKind
	indexRel map[string]model.ObjRef // "schema\x1findex" -> owning relation
	funcs    map[string]bool         // "schema\x1fname" -> routine exists
}

func newResolver(m *model.Database) *resolver {
	r := &resolver{relKind: map[string]model.ObjKind{}, typeKind: map[string]model.ObjKind{}, indexRel: map[string]model.ObjRef{}, funcs: map[string]bool{}}
	if m == nil {
		return r
	}
	for _, s := range m.Schemas {
		for _, t := range s.Tables {
			r.relKind[s.Name+"\x1f"+t.Name] = model.KindTable
			for _, ix := range t.Indexes {
				r.indexRel[s.Name+"\x1f"+ix.Name] = t.Ref()
			}
		}
		for _, v := range s.Views {
			r.relKind[s.Name+"\x1f"+v.Name] = model.KindView
		}
		for _, mv := range s.MatViews {
			r.relKind[s.Name+"\x1f"+mv.Name] = model.KindMatView
			for _, ix := range mv.Indexes {
				r.indexRel[s.Name+"\x1f"+ix.Name] = mv.Ref()
			}
		}
		for _, e := range s.Enums {
			r.typeKind[s.Name+"\x1f"+e.Name] = model.KindEnum
		}
		for _, d := range s.Domains {
			r.typeKind[s.Name+"\x1f"+d.Name] = model.KindDomain
		}
		for _, c := range s.Composites {
			r.typeKind[s.Name+"\x1f"+c.Name] = model.KindComposite
		}
		for _, f := range s.Functions {
			r.funcs[s.Name+"\x1f"+f.Name] = true
		}
	}
	return r
}

// qualify splits name parts into (schema, name); an unqualified name is in
// public, as the default search_path resolves it.
func qualify(parts []string) (string, string) {
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return "public", parts[0]
	default:
		return parts[0], strings.Join(parts[1:], ".")
	}
}

// unit returns the owner unit of a statement, when its head names one object.
func (r *resolver) unit(h sqltext.Head) (string, bool) {
	switch h.Verb {
	case "CREATE", "ALTER", "COMMENT", "GRANT", "REVOKE":
	default:
		return "", false
	}
	switch h.Kind {
	case "INDEX":
		if len(h.On) > 0 {
			return r.relation(h.On)
		}
		// COMMENT ON INDEX schema.name
		s, n := qualify(h.Name)
		if rel, ok := r.indexRel[s+"\x1f"+n]; ok {
			return rel.Key(), true
		}
		return "", false
	case "TRIGGER", "POLICY", "CONSTRAINT", "COLUMN", "RULE":
		if len(h.On) == 0 {
			return "", false
		}
		return r.relation(h.On)
	case "DOMAIN CONSTRAINT":
		s, n := qualify(h.On)
		return model.ObjRef{Kind: model.KindDomain, Schema: s, Name: n}.Key(), true
	case "TABLE", "VIEW", "MATERIALIZED VIEW":
		return r.relation(h.Name)
	case "FUNCTION", "PROCEDURE":
		s, n := qualify(h.Name)
		if s == "" {
			return "", false
		}
		return "routines:" + model.QualifiedName(s, n), true
	case "SEQUENCE":
		s, n := qualify(h.Name)
		return model.ObjRef{Kind: model.KindSequence, Schema: s, Name: n}.Key(), s != ""
	case "TYPE":
		s, n := qualify(h.Name)
		kind, ok := r.typeKind[s+"\x1f"+n]
		if !ok {
			kind = model.KindEnum
		}
		return model.ObjRef{Kind: kind, Schema: s, Name: n}.Key(), s != ""
	case "DOMAIN":
		s, n := qualify(h.Name)
		return model.ObjRef{Kind: model.KindDomain, Schema: s, Name: n}.Key(), s != ""
	case "COLLATION":
		s, n := qualify(h.Name)
		return model.ObjRef{Kind: model.KindCollation, Schema: s, Name: n}.Key(), s != ""
	case "SCHEMA":
		if len(h.Name) == 0 {
			return "", false
		}
		return model.ObjRef{Kind: model.KindSchema, Name: h.Name[0]}.Key(), true
	case "EXTENSION":
		if len(h.Name) == 0 {
			return "", false
		}
		return model.ObjRef{Kind: model.KindExtension, Name: h.Name[0]}.Key(), true
	}
	return "", false
}

// relation resolves a relation name to its table/view/matview key; an unknown
// name is taken as a table (a new object the project does not build yet).
func (r *resolver) relation(parts []string) (string, bool) {
	s, n := qualify(parts)
	if s == "" {
		return "", false
	}
	kind, ok := r.relKind[s+"\x1f"+n]
	if !ok {
		kind = model.KindTable
	}
	return model.ObjRef{Kind: kind, Schema: s, Name: n}.Key(), true
}
