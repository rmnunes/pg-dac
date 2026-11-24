// Package validate proves a generated migration before the user trusts it. It
// materializes the source schema in a throwaway scratch database, replays the
// generated migration against it, then re-introspects and re-diffs against the
// desired model. If the migration fails to execute, that is a generation bug; if
// it executes but leaves a residual difference, the migration does not converge.
package validate

import (
	"context"
	"fmt"

	"github.com/rmnunes/pg-dac/internal/build"
	"github.com/rmnunes/pg-dac/internal/dbconn"
	"github.com/rmnunes/pg-dac/internal/devdb"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// Result reports the outcome of validating a migration.
type Result struct {
	// Converged is true if, after applying the migration, the scratch schema
	// matches the desired model (modulo objects intentionally not dropped per the
	// deploy options).
	Converged bool
	// Residual is the remaining difference after applying the migration. Empty
	// when Converged is true.
	Residual *engine.Result
}

// Plan validates the migration that transforms "from" into "to" under opts.
// A non-nil error means the migration failed to execute (a generation bug or an
// unsupported operation). A nil error with Converged=false means it executed but
// did not fully reach the desired state.
func Plan(ctx context.Context, from, to *model.Database, opts engine.Options) (*Result, error) {
	major := to.MajorVersion()
	if major == 0 {
		major = from.MajorVersion()
	}
	sc, err := devdb.Acquire(ctx, major)
	if err != nil {
		return nil, err
	}
	defer sc.Release()

	conn, err := dbconn.Connect(ctx, sc.ConnString)
	if err != nil {
		return nil, fmt.Errorf("connect to scratch db: %w", err)
	}
	defer conn.Close(ctx)

	// The migration creates objects as the project's owner (SET ROLE); the
	// scratch cluster needs the role, able to do what the deploying role does.
	if opts.Owner != "" {
		if err := build.EnsureOwnerRole(ctx, conn, opts.Owner); err != nil {
			return nil, err
		}
	}

	// Baseline: the empty scratch database (has public, plpgsql, etc.). We build
	// the source schema on top of it so we never try to recreate what is already
	// there.
	baseline, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		return nil, fmt.Errorf("introspect scratch baseline: %w", err)
	}

	// Materialize the source schema (non-online, drops on, so the scratch db ends
	// up exactly matching "from").
	matOpts := engine.Options{Online: false, AllowHazards: true, DropObjectsNotInSource: true, AllowIncompatiblePlatform: true}
	mat, err := engine.Compare(baseline, from, matOpts)
	if err != nil {
		return nil, fmt.Errorf("plan source materialization: %w", err)
	}
	if err := execute.ApplyStatements(ctx, conn, mat.Statements); err != nil {
		return nil, fmt.Errorf("materialize source schema in scratch db: %w", err)
	}
	// What the target has under the owner is under the owner here too, so the
	// migration neither reassigns it nor leaves it to be reassigned.
	if opts.Owner != "" {
		for _, o := range from.OwnedObjects() {
			if o.Owner != opts.Owner || o.Linked {
				continue
			}
			if _, err := conn.Exec(ctx, sqlgen.AlterOwnerSQL(o.Ref, o.Function, opts.Owner)); err != nil {
				return nil, fmt.Errorf("give %s to the owner in scratch db: %w", o.Ref, err)
			}
		}
	}

	// Replay the real migration.
	mig, err := engine.Compare(from, to, opts)
	if err != nil {
		return nil, fmt.Errorf("plan migration: %w", err)
	}
	if err := execute.ApplyStatements(ctx, conn, mig.Statements); err != nil {
		return nil, fmt.Errorf("apply migration to scratch db: %w", err)
	}

	// Re-introspect and check convergence under the same options.
	got, err := introspect.Introspect(ctx, conn, introspect.Options{})
	if err != nil {
		return nil, fmt.Errorf("introspect scratch result: %w", err)
	}
	residual, err := engine.Compare(got, to, opts)
	if err != nil {
		return nil, fmt.Errorf("compute residual: %w", err)
	}
	return &Result{Converged: residual.Empty(), Residual: residual}, nil
}
