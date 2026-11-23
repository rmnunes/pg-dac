// Package build turns authored SQL — a single .sql file or a directory of them
// (a declarative project) — into a canonical schema model. It does this by
// applying the SQL to a throwaway scratch database and introspecting the result,
// so authored DDL canonicalizes through the exact same path as a live database.
// This is what makes a project-vs-database (or, later, artifact-vs-database)
// comparison free of cosmetic false positives.
package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/dbconn"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/sqltext"
)

// Options configures a build.
type Options struct {
	// Schemas restricts the introspected model to these schemas (empty = all
	// user schemas present after applying the SQL).
	Schemas []string
	// TargetMajor selects the scratch PostgreSQL major version so authored SQL is
	// normalized against the version it targets (0 = library default). For a
	// project this is taken from the manifest.
	TargetMajor int
	// Roles are role names the authored SQL grants to without creating (the
	// manifest's `roles:`); they are created in the scratch database first so
	// the GRANTs apply. Roles are cluster objects pg-dac never deploys.
	Roles []string
}

// FromSQL materializes authored SQL in a scratch database and returns its model
// plus any unsupported-object warnings. The model is then brought to its
// canonical fixed point (see canonicalize) so it describes what a database
// published from it will actually contain.
func FromSQL(ctx context.Context, sqlText string, opts Options) (*model.Database, []string, error) {
	db, warns, err := fromSQL(ctx, sqlText, opts)
	if err != nil {
		return nil, nil, err
	}
	fixed, err := Canonicalize(ctx, db, opts)
	if err != nil {
		warns = append(warns, fmt.Sprintf("could not re-materialize the schema from its canonical SQL (%v); "+
			"comparing against the authored form — a publish of this schema will likely fail the same way", err))
		fixed = db
	}
	// Authored SQL describes no cluster: the scratch server's roles are not the
	// model's (a live database keeps its role list for the missing-role check).
	fixed.Roles = nil
	return fixed, warns, nil
}

// Canonicalize brings a model to its canonical fixed point by re-materializing
// it from its own from-empty SQL in a scratch database and re-introspecting,
// until the model stops changing. PostgreSQL's deparsed text is not always a
// fixed point: an authored `c IN ('F','FE')` deparses to
// `= ANY ((ARRAY['F'::varchar, 'FE'::varchar])::text[])`, and parsing THAT text
// yields `= ANY (ARRAY[('F'::varchar)::text, ('FE'::varchar)::text])`. A
// published database holds the second form (pg-dac applies canonical text)
// while a database built by hand from the authored SQL holds the first — so
// EVERY side of a comparison, live databases included, goes through this, or
// the same expression would compare as different depending on how its object
// was created, and on which major version deparsed it. One extra round reaches
// the fixed point in practice; a second is tried if the first still moved.
//
// The scratch major is opts.TargetMajor, else the model's own major. Owners and
// the exact server version — informational fields the renderer does not carry
// — are restored from the input so the result still describes its origin.
// Results are memoized by input hash, so a daemon re-comparing an unchanged
// database pays the scratch round once.
func Canonicalize(ctx context.Context, db *model.Database, opts Options) (*model.Database, error) {
	if opts.TargetMajor == 0 {
		opts.TargetMajor = db.MajorVersion()
	}
	raw, err := db.Marshal()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:]) + "|" + fmt.Sprint(opts.TargetMajor) + "|" + strings.Join(opts.Schemas, ",")
	if hit := canonCache.get(key); hit != nil {
		return hit, nil
	}

	cur := db
	for round := 0; round < 2; round++ {
		next, err := rematerialize(ctx, cur, opts)
		if err != nil {
			return nil, err
		}
		restoreProvenance(db, next)
		a, _ := cur.Marshal()
		b, _ := next.Marshal()
		if bytes.Equal(a, b) {
			break
		}
		cur = next
	}
	canonCache.put(key, cur)
	return cur, nil
}

// canonCache memoizes canonical models (see Canonicalize). Entries are handed
// out as copies so callers can never corrupt a cached model.
var canonCache = &modelCache{entries: map[string]*model.Database{}}

const canonCacheMax = 16

type modelCache struct {
	mu      sync.Mutex
	entries map[string]*model.Database
	order   []string
}

func (c *modelCache) get(key string) *model.Database {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.entries[key]
	if !ok {
		return nil
	}
	return cloneModel(m)
}

func (c *modelCache) put(key string, m *model.Database) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		c.order = append(c.order, key)
		for len(c.order) > canonCacheMax {
			delete(c.entries, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.entries[key] = cloneModel(m)
}

func cloneModel(m *model.Database) *model.Database {
	data, err := m.Marshal()
	if err != nil {
		return m
	}
	cp, err := model.Unmarshal(data)
	if err != nil {
		return m
	}
	return cp
}

// restoreProvenance copies the fields that describe where a model came from —
// the exact server version and object owners — from the source model onto its
// canonical form: the renderer does not emit them, so the scratch database
// would otherwise stamp its own.
func restoreProvenance(src, dst *model.Database) {
	dst.TargetVersion = src.TargetVersion
	dst.Roles = src.Roles
	owners := map[string]string{}
	for _, s := range src.Schemas {
		owners["schema:"+s.Name] = s.Owner
		for _, t := range s.Tables {
			owners[t.Ref().Key()] = t.Owner
		}
		for _, v := range s.Views {
			owners[v.Ref().Key()] = v.Owner
		}
		for _, m := range s.MatViews {
			owners[m.Ref().Key()] = m.Owner
		}
		for _, q := range s.Sequences {
			owners[q.Ref().Key()] = q.Owner
		}
		for _, e := range s.Enums {
			owners[e.Ref().Key()] = e.Owner
		}
		for _, d := range s.Domains {
			owners[d.Ref().Key()] = d.Owner
		}
		for _, c := range s.Composites {
			owners[c.Ref().Key()] = c.Owner
		}
		for _, c := range s.Collations {
			owners[c.Ref().Key()] = c.Owner
		}
	}
	set := func(key string, dst *string) {
		if o, ok := owners[key]; ok {
			*dst = o
		}
	}
	for _, s := range dst.Schemas {
		set("schema:"+s.Name, &s.Owner)
		for _, t := range s.Tables {
			set(t.Ref().Key(), &t.Owner)
		}
		for _, v := range s.Views {
			set(v.Ref().Key(), &v.Owner)
		}
		for _, m := range s.MatViews {
			set(m.Ref().Key(), &m.Owner)
		}
		for _, q := range s.Sequences {
			set(q.Ref().Key(), &q.Owner)
		}
		for _, e := range s.Enums {
			set(e.Ref().Key(), &e.Owner)
		}
		for _, d := range s.Domains {
			set(d.Ref().Key(), &d.Owner)
		}
		for _, c := range s.Composites {
			set(c.Ref().Key(), &c.Owner)
		}
		for _, c := range s.Collations {
			set(c.Ref().Key(), &c.Owner)
		}
	}
}

// rematerialize applies the model's from-empty canonical SQL to a fresh scratch
// database and introspects the result.
func rematerialize(ctx context.Context, db *model.Database, opts Options) (*model.Database, error) {
	sc, err := devdb.Acquire(ctx, opts.TargetMajor)
	if err != nil {
		return nil, err
	}
	defer sc.Release()
	conn, err := dbconn.Connect(ctx, sc.ConnString)
	if err != nil {
		return nil, fmt.Errorf("connect to scratch db: %w", err)
	}
	defer conn.Close(ctx)

	// The model's GRANTs name roles the scratch cluster must have.
	if err := ensureRoles(ctx, conn, db.Grantees()); err != nil {
		return nil, err
	}
	baseline, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		return nil, err
	}
	// Materializing in a scratch server of another major is the point here
	// (normalizing for --target-major), not a deployment to check.
	mig, err := engine.Compare(baseline, db, engine.Options{Online: false, AllowHazards: true, DropObjectsNotInSource: true, AllowIncompatiblePlatform: true})
	if err != nil {
		return nil, err
	}
	if err := execute.ApplyStatements(ctx, conn, mig.Statements); err != nil {
		return nil, err
	}
	return introspect.Introspect(ctx, conn, introspect.Options{IncludeSchemas: opts.Schemas})
}

// fromSQL applies authored SQL to a scratch database and introspects it once
// (the authored form, before canonicalization).
func fromSQL(ctx context.Context, sqlText string, opts Options) (*model.Database, []string, error) {
	sc, err := devdb.Acquire(ctx, opts.TargetMajor)
	if err != nil {
		return nil, nil, err
	}
	defer sc.Release()

	conn, err := dbconn.Connect(ctx, sc.ConnString)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to scratch db: %w", err)
	}
	defer conn.Close(ctx)

	if err := ensureRoles(ctx, conn, opts.Roles); err != nil {
		return nil, nil, err
	}
	// Statements run one by one with deferred retry (see execute.ApplyDeferred),
	// so a project's files need no particular order, and with function-body
	// validation off (as pg_dump does) a function may reference objects that
	// appear later — the scratch database only needs the objects to exist.
	if err := execute.ApplyDeferred(ctx, conn, sqltext.Split(sqlText)); err != nil {
		hint := ""
		if looksLikePsqlScript(sqlText) {
			hint = "\nhint: the source contains psql client-side meta-commands (e.g. \\i, \\echo, \\ir) " +
				"that the server cannot run. Point at the directory holding only schema DDL (e.g. a " +
				"deploy/ subfolder), or add a pgdac.yaml whose include globs list only schema files " +
				"(pre/post-deploy scripts are applied separately and excluded from the schema model)."
		}
		return nil, nil, fmt.Errorf("apply authored SQL to scratch db: %w%s", err, hint)
	}
	return introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: opts.Schemas})
}

// FromDir reads the project's .sql files (the manifest's include globs, else
// every .sql under dir, in sorted path order), concatenates them, and builds a
// model from the result. File order does not affect the model: statements are
// applied with deferred retry, so an object may be defined before what it
// references. The stable order only keeps apply error messages reproducible.
func FromDir(ctx context.Context, dir string, opts Options) (*model.Database, []string, error) {
	files, err := collectFiles(dir, &opts)
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("no .sql files found under %s", dir)
	}
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", f, err)
		}
		b.WriteString("-- file: " + f + "\n")
		b.Write(data)
		b.WriteString("\n")
	}
	return FromSQL(ctx, b.String(), opts)
}

// collectFiles returns the project's schema-body .sql files. When the directory
// has a pgdac.yaml it uses the manifest's ordered include globs (and applies its
// schema scope when the caller did not request specific schemas); otherwise it
// falls back to a recursive, sorted glob of all *.sql.
// looksLikePsqlScript reports whether the text appears to contain psql
// client-side meta-commands (a line whose first non-space character is a
// backslash followed by a letter). Used only to enrich an apply error with a
// hint — never to modify the SQL.
func looksLikePsqlScript(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		t := strings.TrimLeft(line, " \t")
		if len(t) >= 2 && t[0] == '\\' && ((t[1] >= 'a' && t[1] <= 'z') || (t[1] >= 'A' && t[1] <= 'Z')) {
			return true
		}
	}
	return false
}

func collectFiles(dir string, opts *Options) ([]string, error) {
	if project.Exists(dir) {
		m, err := project.Load(dir)
		if err != nil {
			return nil, err
		}
		if len(opts.Schemas) == 0 {
			opts.Schemas = m.Schemas
		}
		if opts.TargetMajor == 0 {
			opts.TargetMajor = m.TargetMajor
		}
		opts.Roles = append(opts.Roles, m.Roles...)
		return m.IncludeFiles(dir)
	}
	return collectSQLFiles(dir)
}

// ensureRoles creates, in the scratch cluster, every role the SQL about to be
// applied grants to. Roles are cluster-wide and the scratch server is shared by
// a daemon's scratch databases, so an existing role is left alone (and a race
// between two scratch builds creating the same role is harmless).
func ensureRoles(ctx context.Context, conn *pgx.Conn, roles []string) error {
	for _, r := range roles {
		if r == "" {
			continue
		}
		sql := "DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = " + model.QuoteLiteral(r) +
			") THEN EXECUTE 'CREATE ROLE ' || " + model.QuoteLiteral(model.QuoteIdent(r)) + " || ' NOLOGIN'; END IF; " +
			"EXCEPTION WHEN duplicate_object THEN NULL; END $$;"
		if _, err := conn.Exec(ctx, sql); err != nil {
			return fmt.Errorf("create role %s in scratch db: %w", r, err)
		}
	}
	return nil
}

func collectSQLFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".sql") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}
