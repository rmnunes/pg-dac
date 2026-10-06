package main

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/rodrigonunes/pg-dac/internal/artifact"
	"github.com/rodrigonunes/pg-dac/internal/deploy"
	"github.com/rodrigonunes/pg-dac/internal/engine"
	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/refactor"
	"github.com/rodrigonunes/pg-dac/internal/source"
	"github.com/rodrigonunes/pg-dac/internal/sqlgen"
	"github.com/rodrigonunes/pg-dac/internal/validate"
)

// commonFlags holds the flags shared by the comparison verbs.
type commonFlags struct {
	from        string
	to          string
	schemas     []string
	targetMajor int
	noOnline    bool
	allowHaz    bool
	dropObjects bool
	doValidate  bool
	requireKey  string

	// dropPolicy is the desired-state source's manifest drop policy, applied as
	// the default unless --drop-objects-not-in-source was given explicitly.
	dropPolicy     string
	dropObjectsSet bool
	plan           *deploy.Plan
}

func (f *commonFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.from, "from", "", "source: the current/target database (connection string)")
	fl.StringVar(&f.to, "to", "", "desired state to converge the target toward (connection string)")
	fl.StringArrayVar(&f.schemas, "schema", nil, "restrict to these schemas (repeatable; default: all user schemas)")
	fl.IntVar(&f.targetMajor, "target-major", 0, "target PostgreSQL major version for version-gated strategies and for normalizing project sources (0 = derive)")
	fl.BoolVar(&f.noOnline, "no-online", false, "disable lock-safe rewrites (CONCURRENTLY, NOT VALID/VALIDATE)")
	fl.BoolVar(&f.allowHaz, "allow-hazards", false, "include data-loss operations (otherwise they are blocked)")
	fl.BoolVar(&f.dropObjects, "drop-objects-not-in-source", false, "drop objects present in the target but not in the source")
	fl.BoolVar(&f.doValidate, "validate", false, "replay the migration in a scratch database to prove it executes and converges")
	fl.StringVar(&f.requireKey, "require-key", "", "fail unless a .pgdac source is signed by this key fingerprint (default: the trusted_keys file, if any)")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
}

// sourceOptions builds the source resolution options, loading the trusted-keys
// file when no explicit key is required.
func (f *commonFlags) sourceOptions() (source.Options, error) {
	opts := source.Options{Schemas: f.schemas, TargetMajor: f.targetMajor, RequireKey: f.requireKey}
	if f.requireKey == "" {
		keys, err := artifact.LoadTrustedKeys()
		if err != nil {
			return opts, fmt.Errorf("read trusted keys: %w", err)
		}
		opts.TrustedKeys = keys
	}
	return opts, nil
}

// loadDeployPlan reads the desired-state source's deployment plan (pre/post
// scripts, drop policy, refactorlog) and records the policy for options().
func (f *commonFlags) loadDeployPlan(cmd *cobra.Command) (*deploy.Plan, error) {
	plan, err := deploy.ForSource(f.to)
	if err != nil {
		return nil, fmt.Errorf("--to: %w", err)
	}
	f.dropPolicy = plan.DropPolicy
	f.dropObjectsSet = cmd.Flags().Changed("drop-objects-not-in-source")
	f.plan = plan
	return plan, nil
}

// applyRefactorLog finds the refactorlog operations the target still needs
// (skipping those it has recorded as applied, when the target is a live
// database), applies them to the target model so the diff sees renames rather
// than drop + create pairs, and returns the statements that perform and record
// them. They are emitted ahead of the migration.
func (f *commonFlags) applyRefactorLog(cmd *cobra.Command, from *model.Database) (*model.Database, []sqlgen.Statement, error) {
	if f.plan == nil || f.plan.Refactor == nil || len(f.plan.Refactor.Operations) == 0 {
		return from, nil, nil
	}
	applied := map[string]bool{}
	if source.IsConnString(f.from) {
		ctx := cmd.Context()
		conn, err := pgx.Connect(ctx, f.from)
		if err != nil {
			return nil, nil, fmt.Errorf("--from: %w", err)
		}
		defer conn.Close(ctx)
		if applied, err = refactor.ReadApplied(ctx, conn); err != nil {
			return nil, nil, err
		}
	}
	renames, warns := refactor.Pending(from, f.plan.Refactor, applied)
	printWarnings(cmd, "refactorlog", warns)
	if len(renames) == 0 {
		return from, nil, nil
	}
	return refactor.ApplyToModel(from, renames), refactor.Statements(renames), nil
}

// resolveModels resolves both sources to models, printing any unsupported-object
// warnings to stderr. The "to" model is desired; "from" is the schema to be changed.
func (f *commonFlags) resolveModels(cmd *cobra.Command) (from, to *model.Database, err error) {
	ctx := cmd.Context()
	srcOpts, err := f.sourceOptions()
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.loadDeployPlan(cmd); err != nil {
		return nil, nil, err
	}
	from, fromWarns, err := source.Resolve(ctx, f.from, srcOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("--from: %w", err)
	}
	to, toWarns, err := source.Resolve(ctx, f.to, srcOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("--to: %w", err)
	}
	printWarnings(cmd, "--from", fromWarns)
	printWarnings(cmd, "--to", toWarns)
	return from, to, nil
}

// printWarnings writes deduplicated unsupported-object warnings for a source to
// stderr, so they never corrupt script/report output on stdout.
func printWarnings(cmd *cobra.Command, label string, warns []string) {
	for _, w := range warns {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning (%s): %s\n", label, w)
	}
}

// compare resolves both sources, applies the desired state's pending
// refactorlog renames to the target model, runs the engine, and — if
// --validate was given — replays the migration in a scratch database, writing
// the outcome to stderr (so it never corrupts a script written to stdout).
func (f *commonFlags) compare(cmd *cobra.Command) (*engine.Result, error) {
	from, to, err := f.resolveModels(cmd)
	if err != nil {
		return nil, err
	}
	from, prelude, err := f.applyRefactorLog(cmd, from)
	if err != nil {
		return nil, err
	}
	res, err := engine.Compare(from, to, f.options())
	if err != nil {
		return nil, err
	}
	res.Prelude = prelude
	printWarnings(cmd, "plan", res.Warnings)
	if f.doValidate {
		f.printValidation(cmd, from, to)
	}
	return res, nil
}

func (f *commonFlags) printValidation(cmd *cobra.Command, from, to *model.Database) {
	w := cmd.ErrOrStderr()
	vr, err := validate.Plan(cmd.Context(), from, to, f.options())
	switch {
	case err != nil:
		fmt.Fprintf(w, "validation: FAILED to apply: %v\n", err)
	case vr.Converged:
		fmt.Fprintln(w, "validation: OK (migration applied to a scratch database and converged to the desired state)")
	default:
		fmt.Fprintf(w, "validation: applied, but %d residual change(s) remain (migration does not fully converge)\n",
			len(vr.Residual.Statements))
	}
}

func (f *commonFlags) options() engine.Options {
	opts := engine.DefaultOptions()
	opts.TargetMajor = f.targetMajor
	opts.Online = !f.noOnline
	opts.AllowHazards = f.allowHaz
	// The source's pgdac.yaml dropPolicy is the default; an explicit flag wins.
	deploy.ApplyDropPolicy(&opts, f.dropPolicy)
	if f.dropObjectsSet || f.dropObjects {
		opts.DropObjectsNotInSource = f.dropObjects
	}
	return opts
}

func newScriptCmd() *cobra.Command {
	f := &commonFlags{}
	cmd := &cobra.Command{
		Use:   "script",
		Short: "Generate the migration SQL that transforms --from into --to",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := f.compare(cmd)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), res.Script(f.allowHaz))
			return nil
		},
	}
	f.bind(cmd)
	return cmd
}

func newReportCmd() *cobra.Command {
	f := &commonFlags{}
	var format string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Produce a machine-readable change report (operations + hazards)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := f.compare(cmd)
			if err != nil {
				return err
			}
			rep := res.Report()
			if format == "json" {
				s, err := rep.JSON()
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), s)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), rep.Text())
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().StringVar(&format, "format", "text", "report format: text | json")
	return cmd
}

func newPlanCmd() *cobra.Command {
	f := &commonFlags{}
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Emit the structured, per-object comparison plan as JSON (for tooling and the GUI)",
		Long: "Produces a PlanDoc: one entry per changed object with its generated SQL,\n" +
			"hazards (with severity), before/after definitions, and dependency edges.\n" +
			"This is the machine-readable form the daemon and GUI consume.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := f.compare(cmd)
			if err != nil {
				return err
			}
			b, err := json.MarshalIndent(res.PlanDoc(), "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
	f.bind(cmd)
	return cmd
}

func newDiffCmd() *cobra.Command {
	f := &commonFlags{}
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Summarize the differences between --from and --to",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := f.compare(cmd)
			if err != nil {
				return err
			}
			if res.Empty() {
				fmt.Fprintln(cmd.OutOrStdout(), "No differences.")
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), res.Report().Text())
			return nil
		},
	}
	f.bind(cmd)
	return cmd
}
