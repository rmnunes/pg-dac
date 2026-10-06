// Package sqlgen renders an ordered plan.Plan into concrete PostgreSQL
// statements, applying online/lock-safe rewrites and attaching the hazards
// classified by internal/safety. It is the only package that produces SQL text;
// the diff and plan packages remain SQL-agnostic.
package sqlgen

import "github.com/rmnunes/pg-dac/internal/safety"

// Statement is one generated SQL statement plus its execution metadata.
type Statement struct {
	// SQL is the statement text, terminated with a semicolon.
	SQL string `json:"sql"`
	// Transactional is false for statements that must NOT run inside a
	// transaction block (CREATE INDEX CONCURRENTLY and the rest of the
	// CONCURRENTLY family, CREATE DATABASE, VACUUM, ...). The executor runs these
	// outside any wrapping transaction.
	Transactional bool `json:"transactional"`
	// Hazards classifies the risks of running this statement.
	Hazards []safety.Hazard `json:"hazards,omitempty"`
	// Note is an optional human-readable explanation rendered as a comment above
	// the statement in script output (e.g. why a constraint is staged NOT VALID).
	Note string `json:"note,omitempty"`
}

// HasHazards reports whether the statement carries any hazard.
func (s Statement) HasHazards() bool { return len(s.Hazards) > 0 }

// hazardousData reports whether any hazard is a data-loss hazard (the gate for
// --allow-hazards / BlockOnPossibleDataLoss).
func (s Statement) hazardousData() bool {
	for _, h := range s.Hazards {
		if h.Type == safety.DeletesData {
			return true
		}
	}
	return false
}

// MaxDataLoss reports whether a slice of statements contains any data-loss hazard.
func MaxDataLoss(stmts []Statement) bool {
	for _, s := range stmts {
		if s.hazardousData() {
			return true
		}
	}
	return false
}
