package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// Script renders the migration as runnable SQL. When allowHazards is false,
// data-loss statements are emitted commented-out with an explanation, so the
// default script is safe to run (the BlockOnPossibleDataLoss posture). Each
// statement is annotated with its note and hazards as SQL comments.
func (r *Result) Script(allowHazards bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- pg-dac migration script\n")
	fmt.Fprintf(&b, "-- %d statement(s)\n", len(r.Statements)+len(r.Prelude))
	if r.Empty() && len(r.Prelude) == 0 {
		b.WriteString("-- (no changes)\n")
		return b.String()
	}
	b.WriteString("-- NOTE: statements are emitted in dependency order; run with autocommit\n")
	b.WriteString("--       (CONCURRENTLY statements must not run inside a transaction).\n")
	b.WriteString("-- Function bodies are not validated at CREATE time (as in pg_dump output), so a\n")
	b.WriteString("-- SQL-language function may reference objects created later in this script.\n")
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "-- WARNING: %s\n", w)
	}
	b.WriteString(SessionPreamble + "\n\n")

	if len(r.Prelude) > 0 {
		b.WriteString("-- refactorlog: renames recorded in the project, applied once per target\n")
		for _, s := range r.Prelude {
			if s.Note != "" {
				fmt.Fprintf(&b, "-- %s\n", s.Note)
			}
			b.WriteString(s.SQL)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	for _, s := range r.Statements {
		if s.Note != "" {
			fmt.Fprintf(&b, "-- %s\n", s.Note)
		}
		for _, h := range s.Hazards {
			fmt.Fprintf(&b, "-- [hazard %s] %s\n", h.Type, h.Message)
		}
		if !s.Transactional {
			b.WriteString("-- (run outside a transaction block)\n")
		}
		if !allowHazards && hasBlocking(s.Hazards) {
			b.WriteString("-- BLOCKED: possible data loss or unsupported change. Re-run with --allow-hazards to include this.\n")
			for _, line := range strings.Split(s.SQL, "\n") {
				fmt.Fprintf(&b, "-- %s\n", line)
			}
			b.WriteString("\n")
			continue
		}
		b.WriteString(s.SQL)
		b.WriteString("\n\n")
	}
	return b.String()
}

// SessionPreamble is the session setting every pg-dac migration relies on. It is
// emitted at the top of scripts and set by the executor before applying: with
// check_function_bodies off, PostgreSQL does not resolve a SQL-language function
// body's references at CREATE time, so functions can be created in the
// "before tables" band even when their bodies query those tables.
const SessionPreamble = "SET check_function_bodies = false;"

// HasDataLoss reports whether the migration contains any data-loss hazard.
func (r *Result) HasDataLoss() bool {
	for _, s := range r.Statements {
		if hasDataLoss(s.Hazards) {
			return true
		}
	}
	return false
}

func hasDataLoss(hz []safety.Hazard) bool {
	for _, h := range hz {
		if h.Type == safety.DeletesData {
			return true
		}
	}
	return false
}

// HasBlocking reports whether the migration contains any error-severity hazard
// (data loss, or a change PostgreSQL cannot apply in place) — the gate a publish
// applies unless hazards are explicitly allowed.
func (r *Result) HasBlocking() bool {
	for _, s := range r.Statements {
		if hasBlocking(s.Hazards) {
			return true
		}
	}
	return false
}

func hasBlocking(hz []safety.Hazard) bool {
	for _, h := range hz {
		if h.Severity == safety.SeverityError {
			return true
		}
	}
	return false
}

// MissingRoles lists the roles the migration grants to that the target lacks
// (the grantees of GRANTs carrying a MISSING_ROLE hazard), sorted.
func (r *Result) MissingRoles() []string {
	seen := map[string]bool{}
	var out []string
	for _, op := range r.ChangeSet.Ops {
		if op.Type == diff.OpGrant && op.MissingRole && op.Grant != nil && !seen[op.Grant.Grantee] {
			seen[op.Grant.Grantee] = true
			out = append(out, op.Grant.Grantee)
		}
	}
	sort.Strings(out)
	return out
}

// OnlyMissingRoles reports whether every blocking hazard of the migration is a
// MISSING_ROLE — the one blocker a project's pre-deploy scripts may remove by
// provisioning the role, so publish runs them and re-checks before refusing.
func (r *Result) OnlyMissingRoles() bool {
	any := false
	for _, s := range r.Statements {
		for _, h := range s.Hazards {
			if h.Severity != safety.SeverityError {
				continue
			}
			if h.Type != safety.MissingRole {
				return false
			}
			any = true
		}
	}
	return any
}

// Report is a machine-readable summary of the migration (the DeployReport analog).
type Report struct {
	StatementCount int          `json:"statementCount"`
	DataLoss       bool         `json:"dataLoss"`
	Operations     []OpSummary  `json:"operations"`
	Hazards        []HazardLine `json:"hazards,omitempty"`
}

// OpSummary names a single planned operation.
type OpSummary struct {
	Action string `json:"action"` // create | drop | alter
	Object string `json:"object"`
}

// HazardLine pairs a statement with its hazards for the report's alerts section.
type HazardLine struct {
	SQL     string          `json:"sql"`
	Hazards []safety.Hazard `json:"hazards"`
}

// Report builds the structured deploy report.
func (r *Result) Report() Report {
	rep := Report{StatementCount: len(r.Statements), DataLoss: r.HasDataLoss()}
	for _, op := range r.Plan.Ops {
		rep.Operations = append(rep.Operations, OpSummary{Action: action(op.Type), Object: object(op)})
	}
	for _, s := range r.Statements {
		if s.HasHazards() {
			rep.Hazards = append(rep.Hazards, HazardLine{SQL: s.SQL, Hazards: s.Hazards})
		}
	}
	return rep
}

// JSON renders the report as indented JSON.
func (rep Report) JSON() (string, error) {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Text renders the report as a human-readable summary.
func (rep Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d statement(s)\n", rep.StatementCount)
	if rep.StatementCount == 0 {
		b.WriteString("No changes.\n")
		return b.String()
	}
	b.WriteString("\nOperations:\n")
	for _, o := range rep.Operations {
		fmt.Fprintf(&b, "  %-7s %s\n", o.Action, o.Object)
	}
	if len(rep.Hazards) > 0 {
		b.WriteString("\nHazards:\n")
		for _, h := range rep.Hazards {
			for _, hz := range h.Hazards {
				fmt.Fprintf(&b, "  [%s] %s\n", hz.Type, hz.Message)
			}
		}
	}
	if rep.DataLoss {
		b.WriteString("\n** This migration can cause DATA LOSS. **\n")
	}
	return b.String()
}

func action(t diff.OpType) string {
	switch {
	case t == diff.OpGrant:
		return "grant"
	case t == diff.OpRevoke:
		return "revoke"
	case t.IsDrop():
		return "drop"
	case strings.HasPrefix(string(t), "create"), strings.HasPrefix(string(t), "add"):
		return "create"
	default:
		return "alter"
	}
}

func object(op diff.Op) string {
	switch {
	case op.Grant != nil:
		who := op.Grant.Grantee
		if who == "" {
			who = "PUBLIC"
		}
		if op.Sub != "" {
			return "privileges of " + who + " on column " + op.Sub + " of " + op.Owner.String()
		}
		return "privileges of " + who + " on " + op.Owner.String()
	case op.Function != nil:
		return "function " + op.Function.QualifiedSignature()
	case op.Trigger != nil:
		return "trigger " + model.QuoteIdent(op.Trigger.Name) + " on " + op.Owner.String()
	case op.Policy != nil:
		return "policy " + model.QuoteIdent(op.Policy.Name) + " on " + op.Owner.String()
	}
	if op.Sub == "" {
		return op.Owner.String()
	}
	var kind string
	switch op.Type {
	case diff.OpAddColumn, diff.OpDropColumn, diff.OpAlterColumnType,
		diff.OpSetColumnDefault, diff.OpDropColumnDefault,
		diff.OpSetColumnNotNull, diff.OpDropColumnNotNull:
		kind = "column"
	case diff.OpAddConstraint, diff.OpDropConstraint, diff.OpValidateConstraint:
		kind = "constraint"
	case diff.OpCreateIndex, diff.OpDropIndex:
		kind = "index"
	default:
		kind = "object"
	}
	return fmt.Sprintf("%s %s on %s", kind, op.Sub, op.Owner.String())
}
