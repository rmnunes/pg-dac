package main

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/rodrigonunes/pg-dac/internal/engine"
	"github.com/rodrigonunes/pg-dac/internal/execute"
	"github.com/rodrigonunes/pg-dac/internal/introspect"
	"github.com/rodrigonunes/pg-dac/internal/source"
	"github.com/rodrigonunes/pg-dac/internal/validate"
)

func newPublishCmd() *cobra.Command {
	f := &commonFlags{}
	var lockTimeout time.Duration
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Apply the migration that brings the --from database up to the desired --to state",
		Long: "Compares the live --from database against the desired --to state and applies the\n" +
			"resulting migration with lock-safety (a session lock_timeout plus jittered\n" +
			"retries). Data-loss operations are refused unless --allow-hazards is given.",
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

			fromModel, fromWarns, err := introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: f.schemas})
			if err != nil {
				return fmt.Errorf("introspect target: %w", err)
			}
			toModel, toWarns, err := source.Resolve(ctx, f.to, source.Options{Schemas: f.schemas, TargetMajor: f.targetMajor})
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
			if res.Empty() {
				fmt.Fprintln(out, "Target is already up to date.")
				return nil
			}

			// Show the plan summary on stderr so stdout stays clean for --dry-run SQL.
			fmt.Fprint(cmd.ErrOrStderr(), res.Report().Text())

			if res.HasDataLoss() && !f.allowHaz {
				return fmt.Errorf("refusing to publish: the migration can cause data loss; re-run with --allow-hazards to proceed")
			}
			if dryRun {
				fmt.Fprint(out, res.Script(f.allowHaz))
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

			execOpts := execute.DefaultOptions()
			execOpts.LockTimeout = lockTimeout
			applied, err := execute.Apply(ctx, conn, res.Statements, execOpts)
			if err != nil {
				return fmt.Errorf("applied %d of %d statement(s) before failure (re-run publish to resume from the new state): %w",
					applied, len(res.Statements), err)
			}
			fmt.Fprintf(out, "Applied %d statement(s).\n", applied)
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().DurationVar(&lockTimeout, "lock-timeout", 3*time.Second, "session lock_timeout for blocking DDL (0 = unset)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration SQL instead of applying it")
	return cmd
}
