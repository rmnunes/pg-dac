package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
)

// ExtractSection is the root folder an extracted project's object files live in.
const ExtractSection = "schema"

// ExtensionsSection is the folder extension files live in.
const ExtensionsSection = "extensions"

// ProjectFile is one object's canonical SQL — its CREATE, sub-objects (keys,
// indexes, triggers, policies), comments and privileges — at the stable path a
// declarative pg-dac project keeps it under:
//
//	extensions/<name>.sql
//	schema/<schema>/schema.sql
//	schema/<schema>/<tables|views|materialized_views|sequences|types|collations|functions|procedures>/<name>.sql
//	schema/<schema>/default_privileges/<role>.<type>.sql
//	schema/default_privileges.<role>.<type>.sql   (database-wide default privileges)
//
// Paths are slash-separated and relative to the project root. The build
// applies statements with deferred retry, so no file order is needed and a
// table's foreign keys can live in the table's own file.
type ProjectFile struct {
	Path  string
	Owner model.ObjRef
	SQL   string
}

// ProjectFiles renders a database as one file per object (see ProjectFile).
// Statements come from the from-empty migration the engine would publish, so
// every object kind pg-dac manages is covered and the text is the canonical
// form a comparison expects. File paths are deterministic; names that would
// collide on a case-insensitive file system or after sanitizing get a short
// hash suffix.
func ProjectFiles(db *model.Database) []ProjectFile {
	baseline := &model.Database{TargetVersion: db.TargetVersion, Schemas: []*model.Schema{{Name: "public"}}}
	res, err := Compare(baseline, db, Options{Online: false})
	if err != nil {
		return nil
	}
	byOwner := map[string]*ProjectFile{}
	var order []string
	for _, g := range res.Groups {
		key := g.Op.Owner.Key()
		pf, ok := byOwner[key]
		if !ok {
			pf = &ProjectFile{Owner: g.Op.Owner, Path: projectPath(g.Op.Owner, routineKind(g.Op))}
			byOwner[key] = pf
			order = append(order, key)
		}
		var sqls []string
		for _, s := range g.Statements {
			sqls = append(sqls, s.SQL)
		}
		if sql := strings.TrimSpace(strings.Join(sqls, "\n")); sql != "" {
			if pf.SQL != "" {
				pf.SQL += "\n"
			}
			pf.SQL += sql
		}
	}
	out := make([]ProjectFile, 0, len(order))
	for _, key := range order {
		if byOwner[key].SQL != "" {
			out = append(out, *byOwner[key])
		}
	}
	disambiguate(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// routineKind reports "procedure" for a routine op, else "".
func routineKind(op diff.Op) string {
	if op.Function != nil {
		return op.Function.Kind
	}
	return ""
}

// projectPath is the stable file path for an object (before collision handling).
func projectPath(o model.ObjRef, routine string) string {
	switch o.Kind {
	case model.KindExtension:
		return ExtensionsSection + "/" + fileSafe(o.Name) + ".sql"
	case model.KindSchema:
		return ExtractSection + "/" + fileSafe(o.Name) + "/schema.sql"
	case model.KindDefaultPrivilege:
		// Name is "<role>/<type>". A database-wide entry lives at the root of the
		// schema folder, where no schema's own folder can collide with it.
		role, typ, _ := strings.Cut(o.Name, "/")
		file := fileSafe(role) + "." + fileSafe(typ) + ".sql"
		if o.Schema == "" {
			return ExtractSection + "/default_privileges." + file
		}
		return ExtractSection + "/" + fileSafe(o.Schema) + "/default_privileges/" + file
	}
	folder := map[model.ObjKind]string{
		model.KindTable:     "tables",
		model.KindView:      "views",
		model.KindMatView:   "materialized_views",
		model.KindSequence:  "sequences",
		model.KindEnum:      "types",
		model.KindDomain:    "types",
		model.KindComposite: "types",
		model.KindCollation: "collations",
		model.KindFunction:  "functions",
	}[o.Kind]
	if folder == "" {
		folder = string(o.Kind)
	}
	name := o.Name
	if o.Kind == model.KindFunction {
		if routine == "procedure" {
			folder = "procedures"
		}
		// Name carries "name(identity args)"; overloads are told apart by a hash
		// of the argument list so the file name stays the routine's name.
		if p := strings.IndexByte(name, '('); p >= 0 {
			args := name[p:]
			name = name[:p]
			if args != "()" {
				name += "__" + shortHash(args)
			}
		}
	}
	return ExtractSection + "/" + fileSafe(o.Schema) + "/" + folder + "/" + fileSafe(name) + ".sql"
}

// disambiguate appends a hash suffix to files whose paths collide once case is
// folded (Windows and macOS file systems) or after sanitizing.
func disambiguate(files []ProjectFile) {
	groups := map[string][]int{}
	for i, f := range files {
		k := strings.ToLower(f.Path)
		groups[k] = append(groups[k], i)
	}
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		for _, i := range idxs {
			p := files[i].Path
			files[i].Path = strings.TrimSuffix(p, ".sql") + "__" + shortHash(files[i].Owner.Key()) + ".sql"
		}
	}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// fileSafe keeps a name usable as a file or folder name on every platform.
func fileSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" || out == "." || out == ".." {
		out = "_" + shortHash(s)
	}
	return out
}
