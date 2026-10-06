package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/rodrigonunes/pg-dac/internal/drift"
	"github.com/rodrigonunes/pg-dac/internal/introspect"
	"github.com/rodrigonunes/pg-dac/internal/source"
)

// exitDrift is the exit code `drift` returns when the target has drifted, so a
// pipeline can gate on it (0 = clean or no baseline, 1 = error, 2 = drift).
const exitDrift = 2

func newDriftCmd() *cobra.Command {
	var from, format string
	var schemas []string
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Compare a database against the baseline recorded by its last publish",
		Long: "Every successful publish records the target's resulting schema as a baseline in\n" +
			"pgdac.baseline. `drift` re-reads the database and reports what changed since —\n" +
			"changes made outside pg-dac. Exit code 2 means drift was detected.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !source.IsConnString(from) {
				return fmt.Errorf("--from must be a database connection string (got %q)", from)
			}
			conn, err := pgx.Connect(ctx, from)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}
			defer conn.Close(ctx)
			current, err := introspect.Introspect(ctx, conn, introspect.Options{IncludeSchemas: schemas})
			if err != nil {
				return fmt.Errorf("introspect: %w", err)
			}
			rep, err := drift.Check(ctx, conn, current)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if format == "json" {
				b, err := json.MarshalIndent(driftJSONOf(rep), "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(w, string(b))
			} else {
				switch {
				case rep.Baseline == nil:
					fmt.Fprintln(w, "No baseline recorded for this target yet (a publish records one; `pg-dac baseline` records one explicitly).")
				case !rep.Drifted:
					fmt.Fprintf(w, "No drift: the target matches baseline #%d recorded %s from %s.\n",
						rep.Baseline.ID, rep.Baseline.RecordedAt.Format(time.RFC3339), rep.Baseline.Source)
				default:
					fmt.Fprintf(w, "DRIFT DETECTED since baseline #%d recorded %s from %s — %d change(s) made outside pg-dac:\n\n",
						rep.Baseline.ID, rep.Baseline.RecordedAt.Format(time.RFC3339), rep.Baseline.Source, len(rep.Changes.Plan.Ops))
					fmt.Fprint(w, rep.Changes.Report().Text())
				}
			}
			if rep.Drifted {
				return &exitError{code: exitDrift}
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&from, "from", "", "database connection string")
	fl.StringArrayVar(&schemas, "schema", nil, "restrict to these schemas (repeatable)")
	fl.StringVar(&format, "format", "text", "output format: text | json")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

func newBaselineCmd() *cobra.Command {
	var from, label string
	var schemas []string
	cmd := &cobra.Command{
		Use:   "baseline",
		Short: "Record the database's current schema as its drift baseline",
		Long: "Records the current schema in pgdac.baseline so later `drift` checks and\n" +
			"publishes compare against it. Useful when adopting pg-dac on an existing database\n" +
			"or to accept changes made outside pg-dac.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !source.IsConnString(from) {
				return fmt.Errorf("--from must be a database connection string (got %q)", from)
			}
			conn, err := pgx.Connect(ctx, from)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}
			defer conn.Close(ctx)
			current, err := introspect.Introspect(ctx, conn, introspect.Options{IncludeSchemas: schemas})
			if err != nil {
				return fmt.Errorf("introspect: %w", err)
			}
			bl, err := drift.Record(ctx, conn, current, label, "pg-dac "+appVersion())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded baseline #%d (%s) for the target.\n", bl.ID, bl.SHA256[:12])
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&from, "from", "", "database connection string")
	fl.StringArrayVar(&schemas, "schema", nil, "restrict to these schemas (repeatable)")
	fl.StringVar(&label, "source", "manual baseline", "label recorded with the baseline")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

// driftJSON is the machine-readable drift report.
type driftJSON struct {
	Drifted       bool            `json:"drifted"`
	CurrentSHA256 string          `json:"currentSha256"`
	Baseline      *baselineJSON   `json:"baseline,omitempty"`
	Changes       json.RawMessage `json:"changes,omitempty"`
}

type baselineJSON struct {
	ID         int64  `json:"id"`
	RecordedAt string `json:"recordedAt"`
	Source     string `json:"source"`
	SHA256     string `json:"sha256"`
}

func driftJSONOf(rep *drift.Report) driftJSON {
	out := driftJSON{Drifted: rep.Drifted, CurrentSHA256: rep.CurrentSHA256}
	if rep.Baseline != nil {
		out.Baseline = &baselineJSON{ID: rep.Baseline.ID, RecordedAt: rep.Baseline.RecordedAt.Format(time.RFC3339), Source: rep.Baseline.Source, SHA256: rep.Baseline.SHA256}
	}
	if rep.Changes != nil {
		if b, err := json.Marshal(rep.Changes.PlanDoc()); err == nil {
			out.Changes = b
		}
	}
	return out
}

// exitError carries a specific process exit code out of a command.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }
