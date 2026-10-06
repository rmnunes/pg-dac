package main

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/rodrigonunes/pg-dac/internal/deploy"
	"github.com/rodrigonunes/pg-dac/internal/engine"
	"github.com/rodrigonunes/pg-dac/internal/execute"
	"github.com/rodrigonunes/pg-dac/internal/introspect"
	"github.com/rodrigonunes/pg-dac/internal/source"
	"github.com/rodrigonunes/pg-dac/internal/validate"
)

func newPublishCmd() *cobra.Command {
	f := &commonFlags{}
	var lockTimeout time.Duration
	var dryRun, noDeployScripts bool

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Apply the migration that brings the --from database up to the desired --to state",
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
				return fmt.Errorf("--from must be a database connection string for publish (got %q)", f.from)
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
			fromModel, fromWarns, err := introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: f.schemas})
			if err != nil {
				return fmt.Errorf("introspect target: %w", err)
			}
			toModel, toWarns, err := source.Resolve(ctx, f.to, srcOpts)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			printWarnings(cmd, "--from", fromWarns)
			printWarnings(cmd, "--to", toWarns)

			opts := f.options()
			res, err := engine.Compare(fromModel, toModel, opts)
			if err != nil {
				return err
			}
			printWarnings(cmd, "plan", res.Warnings)
			pre, post := plan.Pre, plan.Post
			if noDeployScripts {
				pre, post = nil, nil
			}
			if res.Empty() && len(pre) == 0 && len(post) == 0 {
				fmt.Fprintln(out, "Target is already up to date.")
				return nil
			}

			// Show the plan summary on stderr so stdout stays clean for --dry-run SQL.
			fmt.Fprint(cmd.ErrOrStderr(), res.Report().Text())
			if len(pre)+len(post) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "Deploy scripts: %d pre-deploy, %d post-deploy\n", len(pre), len(post))
			}

			if res.HasBlocking() && !f.allowHaz {
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
				vr, verr := validate.Plan(ctx, fromModel, toModel, opts)
				if verr != nil {
					return fmt.Errorf("pre-publish validation failed to apply: %w", verr)
				}
				if !vr.Converged {
					return fmt.Errorf("pre-publish validation did not converge (%d residual change(s)); aborting",
						len(vr.Residual.Statements))
				}
				fmt.Fprintln(cmd.ErrOrStderr(), "validation: OK")
			}

			announce := func(phase string) func(string) {
				return func(name string) { fmt.Fprintf(cmd.ErrOrStderr(), "%s-deploy: %s\n", phase, name) }
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
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().DurationVar(&lockTimeout, "lock-timeout", 3*time.Second, "session lock_timeout for blocking DDL (0 = unset)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration SQL (and deploy scripts) instead of applying")
	cmd.Flags().BoolVar(&noDeployScripts, "no-deploy-scripts", false, "do not run the source's pre/post-deploy scripts")
	return cmd
}
