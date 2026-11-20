// Command pg-dac is a DACPAC/SqlPackage-equivalent for PostgreSQL: it compares
// schemas (database-to-database, and — in later milestones — artifact-to-
// database) and generates dependency-ordered, lock-safe migration scripts.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/devdb"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "0.9.1-dev"

func main() {
	// run() owns all deferred cleanup; main only translates the result into an
	// exit code. Calling os.Exit() directly here would skip deferred cleanup —
	// notably stopping the shared embedded Postgres — and orphan it on error.
	os.Exit(run())
}

func run() int {
	root := &cobra.Command{
		Use:           "pg-dac",
		Short:         "DACPAC-style schema compare and migration for PostgreSQL",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newDiffCmd(), newScriptCmd(), newReportCmd(), newPlanCmd(), newPublishCmd(),
		newBuildCmd(), newExtractCmd(), newInspectCmd(), newServeCmd(), newInitCmd(),
		newRefactorCmd(), newDriftCmd(), newBaselineCmd(), newLintCmd(), newExportCmd(), newImportCmd(),
	)

	// A single shared scratch server is started lazily on first use (if any
	// command needs to normalize a project/.sql source or validate a plan) and
	// stopped when the command finishes — including on error.
	ctx, cleanup := devdb.WithShared(context.Background())
	defer cleanup()

	if err := root.ExecuteContext(ctx); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, "pg-dac: "+ee.msg)
			}
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "pg-dac: "+err.Error())
		return 1
	}
	return 0
}
