package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rodrigonunes/pg-dac/internal/diff"
	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/safety"
)

// Script renders the migration as runnable SQL. When allowHazards is false,
// data-loss statements are emitted commented-out with an explanation, so the
// default script is safe to run (the BlockOnPossibleDataLoss posture). Each
// statement is annotated with its note and hazards as SQL comments.
func (r *Result) Script(allowHazards bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- pg-dac migration script\n")
	fmt.Fprintf(&b, "-- %d statement(s)\n", len(r.Statements))
	if r.Empty() {
		b.WriteString("-- (no changes)\n")
		return b.String()
	}
	b.WriteString("-- NOTE: statements are emitted in dependency order; run with autocommit\n")
	b.WriteString("--       (CONCURRENTLY statements must not run inside a transaction).\n\n")

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
		if !allowHazards && hasDataLoss(s.Hazards) {
			b.WriteString("-- BLOCKED: possible data loss. Re-run with --allow-hazards to include this.\n")
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
