package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/dbconn"
	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/publish"
	"github.com/rmnunes/pg-dac/internal/source"
)

func newPublishCmd() *cobra.Command {
	f := &commonFlags{}
	var lockTimeout, statementTimeout time.Duration
	var dryRun, noDeployScripts, allowDrift, noBaseline, singleTx bool

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
			errOut := cmd.ErrOrStderr()

			if !source.IsConnString(f.from) {
				// A project as the target: update its files from the desired state.
				if project.Exists(strings.TrimPrefix(f.from, "file://")) {
					return publishToProject(cmd, f, dryRun)
				}
				return fmt.Errorf("--from must be a database connection string or a project directory for publish (got %q)", f.from)
			}
			srcOpts, err := f.sourceOptions()
			if err != nil {
				return err
			}
			plan, err := f.loadDeployPlan(cmd)
			if err != nil {
				return err
			}
			overrides, err := f.variableOverrides()
			if err != nil {
				return err
			}
			conn, err := dbconn.Connect(ctx, f.from)
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			execOpts := execute.DefaultOptions()
			execOpts.LockTimeout = lockTimeout
			execOpts.StatementTimeout = statementTimeout
			execOpts.SingleTransaction = singleTx
			req := publish.Request{
				Source:            f.to,
				SourceOptions:     srcOpts,
				Plan:              plan,
				Engine:            f.options(),
				Variables:         overrides,
				SkipDeployScripts: noDeployScripts,
				AllowDrift:        allowDrift,
				RecordBaseline:    !noBaseline,
				Validate:          f.doValidate,
				DryRun:            dryRun,
				Exec:              execOpts,
				Generator:         "pg-dac " + appVersion(),
			}
			// Everything narrative goes to stderr so stdout stays clean for
			// --dry-run SQL.
			obs := publish.Observer{
				Warnings: func(side string, msgs []string) { printWarnings(cmd, flagLabel(side), msgs) },
				Drift: func(msg string) {
					fmt.Fprintln(errOut, "warning (drift): "+msg+" — run `pg-dac drift --from <target>` to see them")
				},
				Rename: func(desc string) { fmt.Fprintf(errOut, "refactorlog: %s\n", desc) },
				Planned: func(res *engine.Result, pre, post []deploy.Script) {
					if res.Empty() && len(pre)+len(post) == 0 {
						return
					}
					fmt.Fprint(errOut, res.Report().Text())
					if len(pre)+len(post) > 0 {
						fmt.Fprintf(errOut, "Deploy scripts: %d pre-deploy, %d post-deploy\n", len(pre), len(post))
					}
				},
				RolesRetry: func(missing []string) {
					fmt.Fprintf(errOut, "roles %v are missing from the target; running the pre-deploy scripts first in case they provision them\n", missing)
				},
				Validated: func() { fmt.Fprintln(errOut, "validation: OK") },
				Script:    func(phase, name string) { fmt.Fprintf(errOut, "%s-deploy: %s\n", phase, name) },
			}
			res, err := publish.Run(ctx, conn, req, obs)
			if err != nil {
				var drifted *publish.DriftError
				var blocked *publish.BlockedError
				switch {
				case errors.As(err, &drifted):
					return fmt.Errorf("refusing to publish: %s; run `pg-dac drift --from <target>` to see them, or re-run with --allow-drift to publish anyway (a fresh baseline is recorded)", drifted)
				case errors.As(err, &blocked) && len(blocked.MissingRoles) > 0:
					return fmt.Errorf("refusing to publish: %s", blocked)
				case errors.As(err, &blocked):
					return fmt.Errorf("refusing to publish: %s; review the hazards and re-run with --allow-hazards to proceed", blocked)
				}
				return err
			}
			switch {
			case res.UpToDate:
				fmt.Fprintln(out, "Target is already up to date.")
			case dryRun:
				for _, s := range res.Pre {
					sql, _ := deploy.Substitute(s.SQL, res.Variables)
					fmt.Fprintf(out, "-- pre-deploy script: %s\n%s\n\n", s.Name, sql)
				}
				fmt.Fprint(out, res.Migration.ScriptWith(engine.ScriptOptions{AllowHazards: f.allowHaz, SingleTransaction: singleTx}))
				for _, s := range res.Post {
					sql, _ := deploy.Substitute(s.SQL, res.Variables)
					fmt.Fprintf(out, "\n-- post-deploy script: %s\n%s\n", s.Name, sql)
				}
				return nil
			default:
				fmt.Fprintf(out, "Applied %d statement(s)", res.Applied)
				if n := res.DeployScripts(); n > 0 {
					fmt.Fprintf(out, " and %d deploy script(s)", n)
				}
				fmt.Fprintln(out, ".")
			}
			if res.Baseline != nil {
				fmt.Fprintf(out, "Recorded baseline #%d.\n", res.Baseline.ID)
			}
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().DurationVar(&lockTimeout, "lock-timeout", 3*time.Second, "session lock_timeout for blocking DDL (0 = unset)")
	cmd.Flags().DurationVar(&statementTimeout, "statement-timeout", 0, "session statement_timeout for every statement (0 = unbounded)")
	cmd.Flags().BoolVar(&singleTx, "single-transaction", false, "run consecutive transactional statements in one transaction (CONCURRENTLY statements split it; with --no-online the whole migration is one transaction)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration SQL (and deploy scripts) instead of applying")
	cmd.Flags().BoolVar(&noDeployScripts, "no-deploy-scripts", false, "do not run the source's pre/post-deploy scripts")
	cmd.Flags().BoolVar(&allowDrift, "allow-drift", false, "publish even if the target changed outside pg-dac since its last baseline")
	cmd.Flags().BoolVar(&noBaseline, "no-baseline", false, "do not record the resulting schema as the target's drift baseline")
	return cmd
}

// flagLabel names a warning side the way the CLI's flags do: the target is
// --from, the desired state --to.
func flagLabel(side string) string {
	switch side {
	case "target":
		return "--from"
	case "source":
		return "--to"
	}
	return side
}
