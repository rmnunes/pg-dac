// Package drift records what a target database looked like right after a
// publish (a baseline) and detects later changes made outside pg-dac — the
// DACPAC "drift" / BlockWhenDriftDetected idea. Baselines live in the target's
// pgdac.baseline table as the canonical model (gzipped JSON) plus its SHA-256,
// so drift can be both detected (hash) and explained (a diff against the
// baseline model).
package drift

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/model"
)

const schemaSQL = `CREATE SCHEMA IF NOT EXISTS pgdac;
CREATE TABLE IF NOT EXISTS pgdac.baseline (
    id           bigserial PRIMARY KEY,
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    source       text NOT NULL,
    generator    text NOT NULL,
    model_sha256 text NOT NULL,
    model        bytea NOT NULL
);`

// Baseline is one recorded expected state of a target.
type Baseline struct {
	ID         int64
	RecordedAt time.Time
	Source     string // what was published (artifact name/version, project path, ...)
	Generator  string
	SHA256     string
	Model      *model.Database
}

// Hash returns the SHA-256 (hex) of the model's canonical JSON.
func Hash(db *model.Database) (string, error) {
	data, err := db.Marshal()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Record stores db as the target's new baseline.
func Record(ctx context.Context, conn *pgx.Conn, db *model.Database, source, generator string) (*Baseline, error) {
	data, err := db.Marshal()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if err := conn.PgConn().Exec(ctx, schemaSQL).Close(); err != nil {
		return nil, fmt.Errorf("create baseline table: %w", err)
	}
	b := &Baseline{Source: source, Generator: generator, SHA256: hex.EncodeToString(sum[:]), Model: db}
	err = conn.QueryRow(ctx,
		`INSERT INTO pgdac.baseline (source, generator, model_sha256, model) VALUES ($1, $2, $3, $4) RETURNING id, recorded_at`,
		source, generator, b.SHA256, buf.Bytes()).Scan(&b.ID, &b.RecordedAt)
	if err != nil {
		return nil, fmt.Errorf("record baseline: %w", err)
	}
	return b, nil
}

// Latest returns the most recent baseline, or nil when the target has none.
func Latest(ctx context.Context, conn *pgx.Conn) (*Baseline, error) {
	var present bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('pgdac.baseline') IS NOT NULL`).Scan(&present); err != nil {
		return nil, fmt.Errorf("check baseline table: %w", err)
	}
	if !present {
		return nil, nil
	}
	b := &Baseline{}
	var blob []byte
	err := conn.QueryRow(ctx,
		`SELECT id, recorded_at, source, generator, model_sha256, model FROM pgdac.baseline ORDER BY id DESC LIMIT 1`).
		Scan(&b.ID, &b.RecordedAt, &b.Source, &b.Generator, &b.SHA256, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read baseline: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("decode baseline: %w", err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("decode baseline: %w", err)
	}
	if b.Model, err = model.Unmarshal(data); err != nil {
		return nil, err
	}
	return b, nil
}

// Report is the outcome of a drift check.
type Report struct {
	// Baseline is nil when the target has never been baselined (no drift can
	// be asserted).
	Baseline *Baseline
	// CurrentSHA256 is the hash of the target's current model.
	CurrentSHA256 string
	// Drifted is true when the target differs from its baseline.
	Drifted bool
	// Changes describes what changed since the baseline (the migration that
	// would turn the baseline into the current state); nil when not drifted.
	Changes *engine.Result
}

// Check compares the target's current model against its latest baseline.
func Check(ctx context.Context, conn *pgx.Conn, current *model.Database) (*Report, error) {
	rep := &Report{}
	var err error
	if rep.CurrentSHA256, err = Hash(current); err != nil {
		return nil, err
	}
	if rep.Baseline, err = Latest(ctx, conn); err != nil {
		return nil, err
	}
	if rep.Baseline == nil || rep.Baseline.SHA256 == rep.CurrentSHA256 {
		return rep, nil
	}
	// Explain the drift as "what happened to the baseline": every drop allowed,
	// no online rewrites, so the report is a plain description.
	changes, err := engine.Compare(rep.Baseline.Model, current, engine.Options{
		Online: false, AllowHazards: true,
		DropObjectsNotInSource: true, DropConstraintsNotInSource: true, DropIndexesNotInSource: true,
	})
	if err != nil {
		return nil, err
	}
	rep.Drifted = !changes.Empty()
	if rep.Drifted {
		rep.Changes = changes
	}
	return rep, nil
}
