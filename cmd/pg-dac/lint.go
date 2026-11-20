package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// exitLint is the exit code `lint` returns when a hazard at or above the
// --fail-on level is present.
const exitLint = 2

// newLintCmd surfaces the migration's hazards as a lint report: every hazard
// code with its severity, count and the objects it touches, and a non-zero
// exit when the migration carries hazards a pipeline should stop on.
func newLintCmd() *cobra.Command {
	f := &commonFlags{}
	var format, failOn string
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Report the migration's hazards (locks, rewrites, data loss, unsupported changes) and fail on them",
		Long: "Generates the migration from --from to --to and lists its hazards grouped by code,\n" +
			"with severity, count and affected objects — the Squawk-style lint for pg-dac's own\n" +
			"generated DDL. Exit code 2 when a hazard at or above --fail-on is present.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := f.compare(cmd)
			if err != nil {
				return err
			}
			rep, err := lintReport(res)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if format == "json" {
				b, err := json.MarshalIndent(rep, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(w, string(b))
			} else {
				if len(rep.Hazards) == 0 {
					fmt.Fprintf(w, "No hazards in %d statement(s).\n", rep.StatementCount)
				}
				for _, h := range rep.Hazards {
					fmt.Fprintf(w, "[%-7s] %-36s %d statement(s)\n", h.Severity, h.Code, h.Count)
					fmt.Fprintf(w, "          %s\n", h.Message)
					for _, o := range h.Objects {
						fmt.Fprintf(w, "          - %s\n", o)
					}
				}
			}
			if lintFails(rep, failOn) {
				return &exitError{code: exitLint, msg: fmt.Sprintf("lint: hazards at or above %q severity present", failOn)}
			}
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().StringVar(&format, "format", "text", "output format: text | json")
	cmd.Flags().StringVar(&failOn, "fail-on", "error", "fail (exit 2) when a hazard of this severity or higher is present: error | warning | none")
	return cmd
}

type lintHazard struct {
	Code     safety.HazardType `json:"code"`
	Severity safety.Severity   `json:"severity"`
	Count    int               `json:"count"`
	Message  string            `json:"message"`
	Objects  []string          `json:"objects"`
}

type lintOutput struct {
	StatementCount int          `json:"statementCount"`
	Hazards        []lintHazard `json:"hazards"`
}

func lintReport(res *engine.Result) (lintOutput, error) {
	out := lintOutput{StatementCount: len(res.Statements) + len(res.Prelude)}
	changes, err := res.ObjectChanges()
	if err != nil {
		return out, err
	}
	byCode := map[safety.HazardType]*lintHazard{}
	seenObj := map[safety.HazardType]map[string]bool{}
	for _, c := range changes {
		for _, s := range c.Statements {
			for _, h := range s.Hazards {
				lh, ok := byCode[h.Type]
				if !ok {
					lh = &lintHazard{Code: h.Type, Severity: h.Severity, Message: h.Message}
					byCode[h.Type] = lh
					seenObj[h.Type] = map[string]bool{}
				}
				lh.Count++
				name := string(c.ObjectType) + " " + c.Name
				if c.Schema != "" {
					name = string(c.ObjectType) + " " + c.Schema + "." + c.Name
				}
				if !seenObj[h.Type][name] {
					seenObj[h.Type][name] = true
					lh.Objects = append(lh.Objects, name)
				}
			}
		}
	}
	for _, lh := range byCode {
		out.Hazards = append(out.Hazards, *lh)
	}
	sort.Slice(out.Hazards, func(i, j int) bool {
		ri, rj := severityOrder(out.Hazards[i].Severity), severityOrder(out.Hazards[j].Severity)
		if ri != rj {
			return ri > rj
		}
		return out.Hazards[i].Code < out.Hazards[j].Code
	})
	return out, nil
}

func severityOrder(s safety.Severity) int {
	switch s {
	case safety.SeverityError:
		return 3
	case safety.SeverityWarning:
		return 2
	case safety.SeverityInfo:
		return 1
	}
	return 0
}

func lintFails(rep lintOutput, failOn string) bool {
	threshold := 0
	switch failOn {
	case "error":
		threshold = 3
	case "warning":
		threshold = 2
	case "info":
		threshold = 1
	default:
		return false
	}
	for _, h := range rep.Hazards {
		if severityOrder(h.Severity) >= threshold {
			return true
		}
	}
	return false
}
