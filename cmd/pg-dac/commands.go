package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/dbconn"
	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/refactor"
	"github.com/rmnunes/pg-dac/internal/source"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
	"github.com/rmnunes/pg-dac/internal/validate"
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

	// Comparison semantics; the desired-state project's compare: block is the
	// default, an explicitly given flag wins (see options()).
	ignorePrivileges  bool
	columnOrder       string
	ignoreComments    bool
	compareWhitespace bool
	excludeTypes      []string
	vars              []string
	cmd               *cobra.Command

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
	fl.BoolVar(&f.ignorePrivileges, "ignore-privileges", false, "do not compare GRANT/REVOKE (recreated objects still get their privileges back)")
	fl.StringVar(&f.columnOrder, "column-order", "", "when a table's declared column order differs from the target's: ignore | warn (default) | rebuild the table in the declared order")
	fl.BoolVar(&f.ignoreComments, "ignore-comments", false, "treat comment-only changes inside routine bodies as no change")
	fl.BoolVar(&f.compareWhitespace, "compare-whitespace", false, "compare routine bodies as exact text (by default whitespace and line endings are ignored)")
	fl.StringArrayVar(&f.excludeTypes, "exclude-object-type", nil, "leave an object type out of the comparison (repeatable): "+strings.Join(project.ObjectTypes, "|"))
	fl.StringArrayVar(&f.vars, "var", nil, "value for a $(name) placeholder in pre/post-deploy scripts, as name=value (repeatable; overrides pgdac.yaml variables:)")
	f.cmd = cmd
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
}

// sourceOptions builds the source resolution options, loading the trusted-keys
// file when no explicit key is required.
func (f *commonFlags) sourceOptions() (source.Options, error) {
	if f.columnOrder != "" && !project.ValidColumnOrder(f.columnOrder) {
		return source.Options{}, fmt.Errorf("--column-order must be one of %v, got %q", project.ColumnOrderPolicies, f.columnOrder)
	}
	for _, t := range f.excludeTypes {
		if !project.ValidObjectType(t) {
			return source.Options{}, fmt.Errorf("--exclude-object-type %q is not one of %v", t, project.ObjectTypes)
		}
	}
	if _, err := f.variableOverrides(); err != nil {
		return source.Options{}, err
	}
	return source.OptionsFor(f.schemas, f.targetMajor, f.requireKey)
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

// pendingRenames applies the desired state's pending refactorlog renames to
// the target model — skipping those a live target has recorded as applied — so
// the diff sees renames rather than drop + create pairs, and returns the
// statements that perform and record them (emitted ahead of the migration)
// together with the refactorlog's warnings.
func pendingRenames(ctx context.Context, fromSpec string, from *model.Database, dplan *deploy.Plan) (*model.Database, []sqlgen.Statement, []string, error) {
	if dplan == nil || dplan.Refactor == nil || len(dplan.Refactor.Operations) == 0 {
		return from, nil, nil, nil
	}
	applied := map[string]bool{}
	if source.IsConnString(fromSpec) {
		conn, err := dbconn.Connect(ctx, fromSpec)
		if err != nil {
			return nil, nil, nil, err
		}
		defer conn.Close(ctx)
		if applied, err = refactor.ReadApplied(ctx, conn); err != nil {
			return nil, nil, nil, err
		}
	}
	renames, warns := refactor.Pending(from, dplan.Refactor, applied)
	if len(renames) == 0 {
		return from, nil, warns, nil
	}
	return refactor.ApplyToModel(from, renames), refactor.Statements(renames), warns, nil
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
	from, prelude, rwarns, err := pendingRenames(cmd.Context(), f.from, from, f.plan)
	if err != nil {
		return nil, err
	}
	printWarnings(cmd, "refactorlog", rwarns)
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
	// The source's pgdac.yaml dropPolicy and compare: block are the defaults; an
	// explicitly given flag wins.
	deploy.ApplyDropPolicy(&opts, f.dropPolicy)
	if f.plan != nil {
		deploy.ApplyCompareSettings(&opts, f.plan.Compare)
	}
	if f.dropObjectsSet || f.dropObjects {
		opts.DropObjectsNotInSource = f.dropObjects
	}
	if f.cmd != nil {
		fl := f.cmd.Flags()
		if fl.Changed("ignore-privileges") {
			opts.IgnorePrivileges = f.ignorePrivileges
		}
		if fl.Changed("column-order") {
			opts.ColumnOrder = f.columnOrder
		}
		if fl.Changed("ignore-comments") {
			opts.IgnoreComments = f.ignoreComments
		}
		if fl.Changed("compare-whitespace") {
			opts.CompareWhitespace = f.compareWhitespace
		}
		if fl.Changed("exclude-object-type") {
			opts.ExcludeObjectTypes = f.excludeTypes
		}
	}
	return opts
}

// variableOverrides parses the --var name=value pairs, which override the
// desired state's pgdac.yaml variables.
func (f *commonFlags) variableOverrides() (map[string]string, error) {
	overrides := map[string]string{}
	for _, kv := range f.vars {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--var expects name=value, got %q", kv)
		}
		overrides[name] = value
	}
	return overrides, nil
}

func newScriptCmd() *cobra.Command {
	f := &commonFlags{}
	var singleTx bool
	cmd := &cobra.Command{
		Use:   "script",
		Short: "Generate the migration SQL that transforms --from into --to",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := f.compare(cmd)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), res.ScriptWith(engine.ScriptOptions{AllowHazards: f.allowHaz, SingleTransaction: singleTx}))
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&singleTx, "single-transaction", false, "wrap runs of transactional statements in BEGIN/COMMIT (CONCURRENTLY statements stay outside)")
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
			doc, err := res.PlanDoc()
			if err != nil {
				return err
			}
			b, err := json.MarshalIndent(doc, "", "  ")
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
