package pgbac

import (
	"compress/gzip"
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/drift"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// ImportOptions configures Import.
type ImportOptions struct {
	RequireKey  string
	TrustedKeys []string
	// NoBaseline skips recording the imported schema as the drift baseline.
	NoBaseline bool
	Generator  string
	// Progress, when set, narrates each phase and table.
	Progress func(msg string)
}

// Report summarizes an import.
type Report struct {
	Tables     int
	Rows       int64
	Statements int
	Warnings   []string
}

// Import recreates the archive's schema and data in the EMPTY database at
// connString (SqlPackage's Import requires an empty target too): pre-data
// objects first, then every table's rows with no keys, indexes or triggers in
// the way, then sequence positions, then keys, indexes, triggers, policies and
// materialized-view contents. Data parts are verified against the manifest
// before they are loaded. The resulting schema is recorded as the target's
// drift baseline.
func Import(ctx context.Context, connString, archivePath string, opts ImportOptions) (*Report, error) {
	o, err := Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer o.Close()
	if !o.IntegrityOK {
		return nil, fmt.Errorf("archive %s failed its integrity check (corrupt or tampered)", archivePath)
	}
	if o.Signed && !o.SignatureOK {
		return nil, fmt.Errorf("archive %s has an invalid signature", archivePath)
	}
	rep := &Report{}
	trustWarns, err := artifact.Trust(archivePath, o.Fingerprint(), o.Signed, opts.RequireKey, opts.TrustedKeys)
	if err != nil {
		return nil, err
	}
	rep.Warnings = append(rep.Warnings, trustWarns...)
	say := func(format string, a ...any) {
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf(format, a...))
		}
	}

	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	raw, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		return nil, fmt.Errorf("introspect target: %w", err)
	}
	if n := occupied(raw, o.Model); n > 0 {
		return nil, fmt.Errorf("the target database is not empty: it already holds %d object(s) in the schemas the archive carries; import needs a fresh database (use publish to migrate an existing one)", n)
	}
	target, err := build.Canonicalize(ctx, raw, build.Options{TargetMajor: o.Model.MajorVersion()})
	if err != nil {
		target = raw
	}

	// The from-empty plan, split into pre-data and post-data like pg_dump.
	res, err := engine.Compare(target, o.Model, engine.Options{Online: false, AllowHazards: true})
	if err != nil {
		return nil, err
	}
	if res.HasBlocking() {
		if missing := res.MissingRoles(); len(missing) > 0 {
			return nil, fmt.Errorf("the archive grants privileges to role(s) %v that do not exist in the target; create them before importing", missing)
		}
		return nil, fmt.Errorf("the schema cannot be created as archived: %s", strings.Join(blockingMessages(res), "; "))
	}
	pre, post := splitPhases(res)
	rep.Statements = len(pre) + len(post)

	say("creating %d pre-data object statement(s)", len(pre))
	if err := execute.ApplyStatements(ctx, conn, pre); err != nil {
		return nil, fmt.Errorf("pre-data: %w", err)
	}

	tables := o.Manifest.Data.Tables
	for _, td := range tables {
		say("loading %s.%s (%d rows)", td.Schema, td.Name, td.Rows)
		if err := o.verifyPart(td.Part); err != nil {
			return nil, err
		}
		rc, err := o.openPart(td.Part)
		if err != nil {
			return nil, err
		}
		gz, err := gzip.NewReader(rc)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("decompress %s: %w", td.Part, err)
		}
		sql := "COPY " + model.QualifiedName(td.Schema, td.Name) + " (" + quoteList(td.Columns) + ") FROM STDIN WITH (FORMAT csv)"
		tag, err := conn.PgConn().CopyFrom(ctx, gz, sql)
		gz.Close()
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("load %s.%s: %w", td.Schema, td.Name, err)
		}
		if tag.RowsAffected() != td.Rows {
			return nil, fmt.Errorf("load %s.%s: %d rows loaded, %d recorded in the archive", td.Schema, td.Name, tag.RowsAffected(), td.Rows)
		}
		rep.Tables++
		rep.Rows += td.Rows
	}

	// Sequence positions (identity sequences located through pg_depend: the
	// server names them, and a renamed source sequence would not match).
	for _, s := range o.Manifest.Data.Sequences {
		expr := model.QuoteLiteral(model.QualifiedName(s.Schema, s.Name)) + "::regclass"
		if col := identityColumnOf(o.Model, s.Schema, s.Name); col != nil {
			expr = "(SELECT d.objid::regclass FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid " +
				"WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.refclassid = 'pg_catalog.pg_class'::regclass AND d.deptype = 'i' " +
				"AND d.refobjid = " + model.QuoteLiteral(model.QualifiedName(col.Schema, col.Table)) + "::regclass AND a.attname = " + model.QuoteLiteral(col.Column) + ")"
		}
		called := "false"
		if s.IsCalled {
			called = "true"
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("SELECT pg_catalog.setval(%s, %d, %s)", expr, s.LastValue, called)); err != nil {
			return nil, fmt.Errorf("restore sequence %s.%s: %w", s.Schema, s.Name, err)
		}
	}

	say("creating %d post-data statement(s) (keys, indexes, triggers, policies)", len(post))
	if err := execute.ApplyStatements(ctx, conn, post); err != nil {
		return nil, fmt.Errorf("post-data: %w", err)
	}

	if !opts.NoBaseline {
		after, err := introspect.Introspect(ctx, conn, introspect.Options{})
		if err != nil {
			return nil, err
		}
		if _, err := drift.Record(ctx, conn, after, archivePath, opts.Generator); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// occupied counts the objects the target already holds in the archive's
// schemas (public counts by content, other schemas by existence).
func occupied(target, m *model.Database) int {
	want := map[string]bool{}
	for _, s := range m.Schemas {
		want[s.Name] = true
	}
	n := 0
	for _, s := range target.Schemas {
		if !want[s.Name] {
			continue
		}
		if s.Name != "public" {
			n++
		}
		n += len(s.Tables) + len(s.Views) + len(s.MatViews) + len(s.Sequences) + len(s.Functions) + len(s.Enums) + len(s.Domains) + len(s.Composites)
	}
	return n
}

// splitPhases divides the from-empty plan: everything that must exist before
// rows are loaded, and everything that is faster or only correct afterwards
// (constraints, indexes, triggers, policies, row security, materialized-view
// contents). Order within each phase is the plan's.
func splitPhases(res *engine.Result) (pre, post []sqlgen.Statement) {
	var refresh []sqlgen.Statement
	for _, g := range res.Groups {
		switch g.Op.Type {
		case diff.OpAddConstraint, diff.OpValidateConstraint, diff.OpCreateIndex,
			diff.OpCreateTrigger, diff.OpSetTriggerEnabled, diff.OpCreatePolicy, diff.OpEnableRLS, diff.OpDisableRLS:
			post = append(post, g.Statements...)
		case diff.OpCreateMatView:
			// Create empty before the rows exist; fill it afterwards.
			for _, s := range g.Statements {
				if strings.HasSuffix(s.SQL, " WITH DATA;") && g.Op.MatView != nil && g.Op.MatView.WithData {
					s.SQL = strings.TrimSuffix(s.SQL, " WITH DATA;") + " WITH NO DATA;"
					refresh = append(refresh, sqlgen.Statement{SQL: "REFRESH MATERIALIZED VIEW " + g.Op.MatView.QualifiedName() + ";", Transactional: true})
				}
				pre = append(pre, s)
			}
		default:
			pre = append(pre, g.Statements...)
		}
	}
	return pre, append(post, refresh...)
}

func blockingMessages(res *engine.Result) []string {
	var out []string
	for _, s := range res.Statements {
		for _, h := range s.Hazards {
			if h.Severity == "error" {
				out = append(out, h.Message)
			}
		}
	}
	return out
}

// identityColumnOf returns the column whose implicit identity sequence the
// named sequence is, or nil for an explicit sequence.
func identityColumnOf(m *model.Database, schema, name string) *model.ColumnRef {
	for _, s := range m.Schemas {
		if s.Name != schema {
			continue
		}
		for _, seq := range s.Sequences {
			if seq.Name == name && m.IsIdentitySequence(seq) {
				return seq.OwnedBy
			}
		}
	}
	return nil
}
