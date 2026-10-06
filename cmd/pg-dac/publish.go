package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/drift"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/refactor"
	"github.com/rmnunes/pg-dac/internal/source"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
	"github.com/rmnunes/pg-dac/internal/validate"
)

func newPublishCmd() *cobra.Command {
	f := &commonFlags{}
	var lockTimeout time.Duration
	var dryRun, noDeployScripts, allowDrift, noBaseline bool

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Apply the migration that brings the --from target (a database, or a project to update) to the desired --to state",
		Long: "Compares the live --from database against the desired --to state and applies the\n" +
			"resulting migration with lock-safety (a session lock_timeout plus jittered\n" +
			"retries). Data-loss and unsupported changes are refused unless --allow-hazards is\n" +
			"given. When the desired state is a project with a pgdac.yaml (or a .pgdac built\n" +
			"from one), its pre-deploy scripts run before the migration and its post-deploy\n" +
			"scripts after it, and its dropPolicy is the default drop behavior.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if !source.IsConnString(f.from) {
				// A project as the target: update its files from the desired state.
				if project.Exists(strings.TrimPrefix(f.from, "file://")) {
					return publishToProject(cmd, f, dryRun)
				}
				return fmt.Errorf("--from must be a database connection string or a project directory for publish (got %q)", f.from)
			}
			conn, err := pgx.Connect(ctx, f.from)
			if err != nil {
				return fmt.Errorf("connect to target: %w", err)
			}
			defer conn.Close(ctx)

			srcOpts, err := f.sourceOptions()
			if err != nil {
				return err
			}
			plan, err := f.loadDeployPlan(cmd)
			if err != nil {
				return err
			}
			// Resolve the desired state first: nothing touches the target until the
			// source is known to build.
			toModel, toWarns, err := source.Resolve(ctx, f.to, srcOpts)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			fromModel, fromWarns, err := introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: f.schemas})
			if err != nil {
				return fmt.Errorf("introspect target: %w", err)
			}
			// Drift: a target that changed outside pg-dac since its last publish is
			// not published to unless the operator says so (BlockWhenDriftDetected).
			drifted, hadBaseline := false, false
			if len(f.schemas) == 0 {
				rep, err := drift.Check(ctx, conn, fromModel)
				if err != nil {
					return err
				}
				drifted, hadBaseline = rep.Drifted, rep.Baseline != nil
				if rep.Drifted {
					msg := fmt.Sprintf("the target has drifted from its last baseline (#%d from %s, %s): %d change(s) were made outside pg-dac — run `pg-dac drift --from <target>` to see them",
						rep.Baseline.ID, rep.Baseline.Source, rep.Baseline.RecordedAt.Format(time.RFC3339), len(rep.Changes.Plan.Ops))
					if !allowDrift && !dryRun {
						return fmt.Errorf("refusing to publish: %s; re-run with --allow-drift to publish anyway (a fresh baseline is recorded)", msg)
					}
					fmt.Fprintln(cmd.ErrOrStderr(), "warning (drift): "+msg)
				}
			}
			// Refactorlog: perform the renames the target still needs, record
			// them, and read the target again so the diff starts from the renamed
			// state (dry runs only transform the model and print the statements).
			var prelude []sqlgen.Statement
			if plan.Refactor != nil && len(plan.Refactor.Operations) > 0 {
				applied, err := refactor.ReadApplied(ctx, conn)
				if err != nil {
					return err
				}
				renames, rwarns := refactor.Pending(fromModel, plan.Refactor, applied)
				printWarnings(cmd, "refactorlog", rwarns)
				if len(renames) > 0 {
					prelude = refactor.Statements(renames)
					if dryRun {
						fromModel = refactor.ApplyToModel(fromModel, renames)
					} else {
						for _, r := range renames {
							fmt.Fprintf(cmd.ErrOrStderr(), "refactorlog: %s\n", r.Op.Describe())
						}
						if err := execute.ApplyStatements(ctx, conn, prelude); err != nil {
							return fmt.Errorf("refactorlog: %w", err)
						}
						prelude = nil
						if fromModel, fromWarns, err = introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: f.schemas}); err != nil {
							return fmt.Errorf("introspect target: %w", err)
						}
					}
				}
			}
			// The raw introspection stays the drift/baseline form; the comparison
			// runs on its canonical form, like every other source.
			canonFrom, canonWarns := source.Canonical(ctx, fromModel, srcOpts)
			printWarnings(cmd, "--from", append(fromWarns, canonWarns...))
			printWarnings(cmd, "--to", toWarns)

			opts := f.options()
			res, err := engine.Compare(canonFrom, toModel, opts)
			if err != nil {
				return err
			}
			res.Prelude = prelude
			printWarnings(cmd, "plan", res.Warnings)
			pre, post := plan.Pre, plan.Post
			if noDeployScripts {
				pre, post = nil, nil
			}
			if res.Empty() && len(pre) == 0 && len(post) == 0 {
				fmt.Fprintln(out, "Target is already up to date.")
				if !dryRun && !noBaseline && len(f.schemas) == 0 && (drifted || !hadBaseline) {
					// Nothing to migrate, but the target has no baseline yet (or its
					// drift was just accepted): record the current state as the baseline.
					bl, err := drift.Record(ctx, conn, fromModel, f.to, "pg-dac "+appVersion())
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "Recorded baseline #%d.\n", bl.ID)
				}
				return nil
			}

			// Show the plan summary on stderr so stdout stays clean for --dry-run SQL.
			fmt.Fprint(cmd.ErrOrStderr(), res.Report().Text())
			if len(pre)+len(post) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "Deploy scripts: %d pre-deploy, %d post-deploy\n", len(pre), len(post))
			}

			announce := func(phase string) func(string) {
				return func(name string) { fmt.Fprintf(cmd.ErrOrStderr(), "%s-deploy: %s\n", phase, name) }
			}
			// Roles are the environment's: a project provisions them in a pre-deploy
			// script. When missing roles are the only blocker, run those scripts and
			// re-check before refusing — nothing of the schema has been touched yet.
			if res.HasBlocking() && !f.allowHaz && !dryRun && res.OnlyMissingRoles() && len(pre) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "roles %v are missing from the target; running the pre-deploy scripts first in case they provision them\n", res.MissingRoles())
				if err := deploy.Run(ctx, conn, pre, announce("pre")); err != nil {
					return err
				}
				pre = nil
				if canonFrom.Roles, err = introspect.ReadRoles(ctx, conn); err != nil {
					return err
				}
				if res, err = engine.Compare(canonFrom, toModel, opts); err != nil {
					return err
				}
				res.Prelude = prelude
			}
			if res.HasBlocking() && !f.allowHaz {
				if missing := res.MissingRoles(); len(missing) > 0 {
					return fmt.Errorf("refusing to publish: the source grants privileges to role(s) %v that do not exist in the target; create them (a pre-deploy script is the place) or remove the grants", missing)
				}
				return fmt.Errorf("refusing to publish: the migration contains data-loss or unsupported changes; review the hazards and re-run with --allow-hazards to proceed")
			}
			if dryRun {
				for _, s := range pre {
					fmt.Fprintf(out, "-- pre-deploy script: %s\n%s\n\n", s.Name, s.SQL)
				}
				fmt.Fprint(out, res.Script(f.allowHaz))
				for _, s := range post {
					fmt.Fprintf(out, "\n-- post-deploy script: %s\n%s\n", s.Name, s.SQL)
				}
				return nil
			}
			if f.doValidate {
				vr, verr := validate.Plan(ctx, canonFrom, toModel, opts)
				if verr != nil {
					return fmt.Errorf("pre-publish validation failed to apply: %w", verr)
				}
				if !vr.Converged {
					return fmt.Errorf("pre-publish validation did not converge (%d residual change(s)); aborting",
						len(vr.Residual.Statements))
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "validation: OK")
			}

			if err := deploy.Run(ctx, conn, pre, announce("pre")); err != nil {
				return err
			}
			execOpts := execute.DefaultOptions()
			execOpts.LockTimeout = lockTimeout
			applied, err := execute.Apply(ctx, conn, res.Statements, execOpts)
			if err != nil {
				return fmt.Errorf("applied %d of %d statement(s) before failure (re-run publish to resume from the new state): %w",
					applied, len(res.Statements), err)
			}
			if err := deploy.Run(ctx, conn, post, announce("post")); err != nil {
				return err
			}
			fmt.Fprintf(out, "Applied %d statement(s)", applied)
			if len(pre)+len(post) > 0 {
				fmt.Fprintf(out, " and %d deploy script(s)", len(pre)+len(post))
			}
			fmt.Fprintln(out, ".")
			// Record the resulting schema as the target's baseline for drift checks.
			if !noBaseline && len(f.schemas) == 0 {
				after, err := introspect.Introspect(ctx, conn, introspect.Options{})
				if err != nil {
					return fmt.Errorf("introspect target for baseline: %w", err)
				}
				bl, err := drift.Record(ctx, conn, after, f.to, "pg-dac "+appVersion())
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Recorded baseline #%d.\n", bl.ID)
			}
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().DurationVar(&lockTimeout, "lock-timeout", 3*time.Second, "session lock_timeout for blocking DDL (0 = unset)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration SQL (and deploy scripts) instead of applying")
	cmd.Flags().BoolVar(&noDeployScripts, "no-deploy-scripts", false, "do not run the source's pre/post-deploy scripts")
	cmd.Flags().BoolVar(&allowDrift, "allow-drift", false, "publish even if the target changed outside pg-dac since its last baseline")
	cmd.Flags().BoolVar(&noBaseline, "no-baseline", false, "do not record the resulting schema as the target's drift baseline")
	return cmd
}
