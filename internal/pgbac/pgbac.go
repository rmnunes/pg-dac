// Package pgbac reads and writes the .pgbac archive — pg-dac's BACPAC: the
// canonical schema model of a database plus its table data, as one portable,
// integrity-checked, optionally signed file. Export reads everything in a
// single REPEATABLE READ transaction so schema and data are one consistent
// snapshot; Import recreates the schema in an empty database the way a
// publish would (pre-data objects, then the rows, then keys, indexes,
// triggers and policies, like pg_dump's sections), restores sequence
// positions and records a drift baseline.
//
// Layout (a ZIP archive, sharing the .pgdac manifest):
//
//	manifest.json                 artifact.Manifest with kind "pgbac" and a data block
//	model.json                    the canonical schema model
//	sql/schema.sql                canonical CREATE script, for human review
//	data/<schema>/<table>.csv.gz  one gzip CSV per table (generated columns excluded)
//
// Data parts are hashed and listed in the manifest like every other part, so
// the Merkle root and the signature cover the data; they are verified while
// streaming at import time rather than loaded into memory.
package pgbac

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
)

// Kind is the manifest kind of a .pgbac.
const Kind = "pgbac"

// DataDir is the archive folder data parts live under.
const DataDir = "data/"

// ExportOptions configures Export.
type ExportOptions struct {
	Name, Version, Generator string
	// Schemas restricts the export (empty = all user schemas).
	Schemas []string
	// Tables / ExcludeTables are "schema.table" globs (path.Match syntax)
	// selecting which tables' DATA is exported; the schema is always complete.
	Tables, ExcludeTables []string
	// TargetMajor selects the scratch major used to canonicalize the model.
	TargetMajor int
	// SchemaSQL is embedded as sql/schema.sql when non-empty.
	SchemaSQL  func(*model.Database) string
	SigningKey ed25519.PrivateKey
	// Progress, when set, is told about each table as it is written.
	Progress func(table string, rows int64)
}

// Export writes the schema and data of the database at connString to path.
// Returns the manifest and any introspection warnings.
func Export(ctx context.Context, connString, outPath string, opts ExportOptions) (*artifact.Manifest, []string, error) {
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	// One snapshot for the catalog and every table.
	if _, err := conn.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return nil, nil, fmt.Errorf("begin snapshot transaction: %w", err)
	}
	defer conn.Exec(context.Background(), "ROLLBACK") //nolint:errcheck // best effort if we fail midway
	snapshot := time.Now().UTC().Format(time.RFC3339)

	raw, warns, err := introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: opts.Schemas})
	if err != nil {
		return nil, nil, fmt.Errorf("introspect: %w", err)
	}
	m, err := build.Canonicalize(ctx, raw, build.Options{Schemas: opts.Schemas, TargetMajor: opts.TargetMajor})
	if err != nil {
		warns = append(warns, fmt.Sprintf("could not canonicalize the schema through a scratch PostgreSQL (%v); the archive carries the raw catalog form", err))
		m = raw
	}

	f, err := os.Create(outPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	var parts []artifact.Part
	addPart := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		parts = append(parts, artifact.Part{Name: name, SHA256: artifact.SHA256Hex(data)})
		return nil
	}

	// Table data: leaf tables only (a partitioned parent's rows live in its
	// partitions), copied column by column so generated values are recomputed.
	data := &artifact.DataInfo{Snapshot: snapshot}
	names := partNames(m)
	for _, t := range leafTables(m) {
		full := t.Schema + "." + t.Name
		if !selected(full, opts.Tables, opts.ExcludeTables) {
			continue
		}
		cols := copiedColumns(t)
		if len(cols) == 0 {
			continue
		}
		part := names[t.Ref().Key()]
		hdr := &zip.FileHeader{Name: part, Method: zip.Store} // already gzip-compressed
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, nil, err
		}
		hw := &hashWriter{w: w, h: sha256.New()}
		gz := gzip.NewWriter(hw)
		sql := "COPY " + t.QualifiedName() + " (" + quoteList(cols) + ") TO STDOUT WITH (FORMAT csv)"
		tag, err := conn.PgConn().CopyTo(ctx, gz, sql)
		if err != nil {
			return nil, nil, fmt.Errorf("export %s: %w", full, err)
		}
		if err := gz.Close(); err != nil {
			return nil, nil, err
		}
		data.Tables = append(data.Tables, artifact.TableData{Schema: t.Schema, Name: t.Name, Part: part, Columns: cols, Rows: tag.RowsAffected()})
		parts = append(parts, artifact.Part{Name: part, SHA256: hex.EncodeToString(hw.h.Sum(nil))})
		if opts.Progress != nil {
			opts.Progress(full, tag.RowsAffected())
		}
	}
	// Sequence positions.
	for _, s := range m.Schemas {
		for _, seq := range s.Sequences {
			var last int64
			var called bool
			if err := conn.QueryRow(ctx, "SELECT last_value, is_called FROM "+seq.QualifiedName()).Scan(&last, &called); err != nil {
				return nil, nil, fmt.Errorf("read sequence %s: %w", seq.QualifiedName(), err)
			}
			data.Sequences = append(data.Sequences, artifact.SequenceState{Schema: seq.Schema, Name: seq.Name, LastValue: last, IsCalled: called})
		}
	}
	if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
		return nil, nil, err
	}

	modelBytes, err := m.Marshal()
	if err != nil {
		return nil, nil, err
	}
	if err := addPart(artifact.PartModel, modelBytes); err != nil {
		return nil, nil, err
	}
	if opts.SchemaSQL != nil {
		if sql := opts.SchemaSQL(m); sql != "" {
			if err := addPart(artifact.PartSchema, []byte(sql)); err != nil {
				return nil, nil, err
			}
		}
	}
	man := &artifact.Manifest{
		FormatVersion: artifact.FormatVersion,
		Kind:          Kind,
		Name:          opts.Name,
		Version:       opts.Version,
		Generator:     opts.Generator,
		TargetMajor:   m.MajorVersion(),
		CreatedAt:     snapshot,
		Data:          data,
		Parts:         parts,
	}
	if err := artifact.Seal(man, opts.SigningKey); err != nil {
		return nil, nil, err
	}
	manBytes, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	w, err := zw.Create(artifact.PartManifest)
	if err != nil {
		return nil, nil, err
	}
	if _, err := w.Write(manBytes); err != nil {
		return nil, nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, nil, err
	}
	return man, warns, nil
}

// leafTables returns the tables whose rows are stored directly (partitions and
// plain tables), sorted.
func leafTables(m *model.Database) []*model.Table {
	var out []*model.Table
	for _, s := range m.Schemas {
		for _, t := range s.Tables {
			if t.PartitionKey == "" {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// copiedColumns lists a table's columns in attribute order, without generated
// ones (PostgreSQL computes them and COPY cannot set them).
func copiedColumns(t *model.Table) []string {
	var cols []string
	for _, c := range t.Columns {
		if c.Generated == "" {
			cols = append(cols, c.Name)
		}
	}
	return cols
}

// partNames assigns every leaf table a stable data part name, with a hash
// suffix where sanitized names would collide.
func partNames(m *model.Database) map[string]string {
	type entry struct {
		key, name string
	}
	var entries []entry
	for _, t := range leafTables(m) {
		entries = append(entries, entry{t.Ref().Key(), DataDir + safe(t.Schema) + "/" + safe(t.Name) + ".csv.gz"})
	}
	byLower := map[string]int{}
	for _, e := range entries {
		byLower[strings.ToLower(e.name)]++
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.name
		if byLower[strings.ToLower(e.name)] > 1 {
			sum := sha256.Sum256([]byte(e.key))
			name = strings.TrimSuffix(name, ".csv.gz") + "__" + hex.EncodeToString(sum[:4]) + ".csv.gz"
		}
		out[e.key] = name
	}
	return out
}

func safe(s string) string {
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
	if out == "" {
		sum := sha256.Sum256([]byte(s))
		out = "_" + hex.EncodeToString(sum[:4])
	}
	return out
}

// selected applies include/exclude globs to "schema.table".
func selected(full string, include, exclude []string) bool {
	if len(include) > 0 {
		hit := false
		for _, g := range include {
			if ok, _ := path.Match(g, full); ok {
				hit = true
			}
		}
		if !hit {
			return false
		}
	}
	for _, g := range exclude {
		if ok, _ := path.Match(g, full); ok {
			return false
		}
	}
	return true
}

func quoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = model.QuoteIdent(c)
	}
	return strings.Join(q, ", ")
}

type hashWriter struct {
	w io.Writer
	h hash.Hash
}

func (h *hashWriter) Write(p []byte) (int, error) {
	h.h.Write(p)
	return h.w.Write(p)
}
