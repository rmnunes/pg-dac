// Package publish applies a desired state to a live database: the one
// pipeline behind `pg-dac publish` and the GUI's Publish. It reads the target,
// honours its drift baseline, performs the pending refactorlog renames, runs
// the comparison, refuses blocking hazards (after giving a pre-deploy script
// the chance to provision missing roles), runs the pre-deploy scripts, the
// migration and the post-deploy scripts, and records the resulting schema as
// the target's new baseline. Callers observe progress through Observer and
// decide how to present it; the decisions themselves live here, once.
package publish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/drift"
	"github.com/rmnunes/pg-dac/internal/engine"
	"github.com/rmnunes/pg-dac/internal/execute"
	"github.com/rmnunes/pg-dac/internal/introspect"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/projsync"
	"github.com/rmnunes/pg-dac/internal/refactor"
	"github.com/rmnunes/pg-dac/internal/source"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
	"github.com/rmnunes/pg-dac/internal/validate"
)

// Request describes one publish.
type Request struct {
	// Source is the desired-state spec (the engine's "to"): a project, a .sql
	// file, a .pgdac or a connection string.
	Source string
	// SourceOptions resolves and canonicalizes both sides.
	SourceOptions source.Options
	// Plan is the desired state's deployment content (deploy scripts, drop
	// policy, refactorlog, variables); never nil.
	Plan *deploy.Plan
	// Engine holds the comparison options with the deployment plan's defaults
	// already applied.
	Engine engine.Options
	// Variables override the plan's values for the deploy scripts' $(name)
	// placeholders.
	Variables map[string]string
	// SkipDeployScripts leaves the pre/post-deploy scripts out.
	SkipDeployScripts bool
	// AllowDrift publishes to a target that changed outside pg-dac since its
	// last baseline (BlockWhenDriftDetected off).
	AllowDrift bool
	// RecordBaseline records the resulting schema as the target's drift
	// baseline (only when the whole database is in scope).
	RecordBaseline bool
	// Validate replays the migration in a scratch database before applying it.
	Validate bool
	// DryRun stops before anything runs against the target: the result carries
	// the migration and scripts for the caller to print. Refactorlog renames are
	// applied to the model only.
	DryRun bool
	// Exec tunes lock safety and transaction batching.
	Exec execute.Options
	// Generator is recorded with the baseline ("pg-dac <version>").
	Generator string
}

// Observer receives progress; every field is optional.
type Observer struct {
	// Warnings reports the warnings of one side: "target", "source",
	// "refactorlog" or "plan".
	Warnings func(side string, msgs []string)
	// Drift reports drift that is being published over (AllowDrift).
	Drift func(msg string)
	// Rename reports a refactorlog rename about to run on the target.
	Rename func(description string)
	// Planned reports the migration and the deploy scripts that will run,
	// before anything runs. res may be empty.
	Planned func(res *engine.Result, pre, post []deploy.Script)
	// RolesRetry reports that the pre-deploy scripts run ahead of the hazard
	// check because missing roles are the only blocker.
	RolesRetry func(missing []string)
	// Validated reports a successful pre-publish validation.
	Validated func()
	// Script reports a deploy script ("pre" or "post") about to run.
	Script func(phase, name string)
	// Statement reports each migration statement about to run.
	Statement execute.Progress
}

func (o Observer) warn(side string, msgs []string) {
	if o.Warnings != nil && len(msgs) > 0 {
		o.Warnings(side, msgs)
	}
}

// Result is the outcome of a publish.
type Result struct {
	// Migration is the comparison that was (or, for a dry run, would be)
	// applied, with its prelude.
	Migration *engine.Result
	// Pre and Post are the deploy scripts that ran (or would run), Variables
	// the values their placeholders resolved to.
	Pre, Post []deploy.Script
	Variables map[string]string
	// UpToDate is true when nothing had to run.
	UpToDate bool
	// Applied is the number of migration statements applied.
	Applied int
	// Baseline is the baseline recorded afterwards, nil when none was.
	Baseline *drift.Baseline
}

// DeployScripts is the number of deploy scripts of the result.
func (r *Result) DeployScripts() int { return len(r.Pre) + len(r.Post) }

// DriftError refuses a target that changed outside pg-dac since its baseline.
type DriftError struct {
	Baseline *drift.Baseline
	Changes  int
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("the target has drifted from its last baseline (#%d from %s, %s): %d change(s) were made outside pg-dac",
		e.Baseline.ID, e.Baseline.Source, e.Baseline.RecordedAt.Format(time.RFC3339), e.Changes)
}

// BlockedError refuses a migration with blocking hazards.
type BlockedError struct {
	// MissingRoles, when non-empty, is the only blocker: grants to roles the
	// target lacks.
	MissingRoles []string
}

func (e *BlockedError) Error() string {
	if len(e.MissingRoles) > 0 {
		return fmt.Sprintf("the source grants privileges to role(s) %v that do not exist in the target; create them (a pre-deploy script is the place) or remove the grants", e.MissingRoles)
	}
	return "the migration contains data-loss or unsupported changes"
}

// ApplyError reports a migration that stopped part-way; the target holds the
// statements applied so far, and publishing again resumes from that state.
type ApplyError struct {
	Applied, Total int
	Err            error
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("applied %d of %d statement(s) before failure (publish again to resume from the new state): %v", e.Applied, e.Total, e.Err)
}

func (e *ApplyError) Unwrap() error { return e.Err }

// BaselineError reports a migration that applied but whose baseline could not
// be recorded afterwards.
type BaselineError struct {
	Applied int
	Err     error
}

func (e *BaselineError) Error() string {
	return fmt.Sprintf("the migration applied (%d statement(s)) but the baseline could not be recorded: %v", e.Applied, e.Err)
}

func (e *BaselineError) Unwrap() error { return e.Err }

// Run publishes req to the database behind conn.
func Run(ctx context.Context, conn *pgx.Conn, req Request, obs Observer) (*Result, error) {
	if req.Plan == nil {
		req.Plan = &deploy.Plan{}
	}
	schemas := req.SourceOptions.Schemas
	wholeDB := len(schemas) == 0

	// Resolve the desired state first: nothing touches the target until the
	// source is known to build.
	toModel, toWarns, err := source.Resolve(ctx, req.Source, req.SourceOptions)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	obs.warn("source", toWarns)
	fromModel, fromWarns, err := introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: schemas})
	if err != nil {
		return nil, fmt.Errorf("introspect target: %w", err)
	}

	// Drift: a target that changed outside pg-dac since its last publish is
	// not published to unless the operator says so (BlockWhenDriftDetected).
	drifted, hadBaseline := false, false
	if wholeDB {
		rep, err := drift.Check(ctx, conn, fromModel)
		if err != nil {
			return nil, err
		}
		drifted, hadBaseline = rep.Drifted, rep.Baseline != nil
		if rep.Drifted {
			derr := &DriftError{Baseline: rep.Baseline, Changes: len(rep.Changes.Plan.Ops)}
			if !req.AllowDrift && !req.DryRun {
				return nil, derr
			}
			if obs.Drift != nil {
				obs.Drift(derr.Error())
			}
		}
	}

	// Refactorlog: perform the renames the target still needs, record them,
	// and read the target again so the diff starts from the renamed state. A
	// dry run only transforms the model and keeps the statements as prelude.
	var prelude []sqlgen.Statement
	if req.Plan.Refactor != nil && len(req.Plan.Refactor.Operations) > 0 {
		applied, err := refactor.ReadApplied(ctx, conn)
		if err != nil {
			return nil, err
		}
		renames, rwarns := refactor.Pending(fromModel, req.Plan.Refactor, applied)
		obs.warn("refactorlog", rwarns)
		if len(renames) > 0 {
			if req.DryRun {
				fromModel = refactor.ApplyToModel(fromModel, renames)
				prelude = refactor.Statements(renames)
			} else {
				for _, r := range renames {
					if obs.Rename != nil {
						obs.Rename(r.Op.Describe())
					}
				}
				if err := execute.ApplyStatements(ctx, conn, refactor.Statements(renames)); err != nil {
					return nil, fmt.Errorf("refactorlog: %w", err)
				}
				if fromModel, fromWarns, err = introspect.IntrospectWithWarnings(ctx, conn, introspect.Options{IncludeSchemas: schemas}); err != nil {
					return nil, fmt.Errorf("introspect target: %w", err)
				}
			}
		}
	}

	// The raw introspection stays the drift/baseline form; the comparison runs
	// on its canonical form, like every other source.
	canonFrom, canonWarns := source.Canonical(ctx, fromModel, req.SourceOptions)
	obs.warn("target", append(fromWarns, canonWarns...))

	res, err := engine.Compare(canonFrom, toModel, req.Engine)
	if err != nil {
		return nil, err
	}
	res.Prelude = prelude
	obs.warn("plan", res.Warnings)

	pre, post := req.Plan.Pre, req.Plan.Post
	if req.SkipDeployScripts {
		pre, post = nil, nil
	}
	vars := deploy.MergeVariables(req.Plan.Variables, req.Variables)
	// Every placeholder must resolve before anything runs.
	for _, s := range append(append([]deploy.Script{}, pre...), post...) {
		if _, err := deploy.Substitute(s.SQL, vars); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	if obs.Planned != nil {
		obs.Planned(res, pre, post)
	}
	out := &Result{Migration: res, Pre: pre, Post: post, Variables: vars}

	if res.Empty() && len(pre)+len(post) == 0 {
		out.UpToDate = true
		// Nothing to migrate, but the target has no baseline yet (or its drift
		// was just accepted): record the current state as the baseline.
		if req.RecordBaseline && wholeDB && (drifted || !hadBaseline) && !req.DryRun {
			if out.Baseline, err = drift.Record(ctx, conn, fromModel, req.Source, req.Generator); err != nil {
				return out, &BaselineError{Err: err}
			}
		}
		return out, nil
	}

	// Roles are the environment's: a project provisions them in a pre-deploy
	// script. When missing roles are the only blocker, run those scripts and
	// re-check before refusing — nothing of the schema has been touched yet.
	if res.HasBlocking() && !req.Engine.AllowHazards && !req.DryRun && res.OnlyMissingRoles() && len(pre) > 0 {
		if obs.RolesRetry != nil {
			obs.RolesRetry(res.MissingRoles())
		}
		if err := deploy.Run(ctx, conn, pre, vars, scriptHook(obs, "pre")); err != nil {
			return out, err
		}
		pre = nil
		out.Pre = nil
		if canonFrom.Roles, err = introspect.ReadRoles(ctx, conn); err != nil {
			return out, err
		}
		if res, err = engine.Compare(canonFrom, toModel, req.Engine); err != nil {
			return out, err
		}
		res.Prelude = prelude
		out.Migration = res
	}
	if res.HasBlocking() && !req.Engine.AllowHazards {
		return out, &BlockedError{MissingRoles: res.MissingRoles()}
	}
	if req.DryRun {
		return out, nil
	}
	if req.Validate {
		vr, err := validate.Plan(ctx, canonFrom, toModel, req.Engine)
		if err != nil {
			return out, fmt.Errorf("pre-publish validation failed to apply: %w", err)
		}
		if !vr.Converged {
			return out, fmt.Errorf("pre-publish validation did not converge (%d residual change(s)); aborting", len(vr.Residual.Statements))
		}
		if obs.Validated != nil {
			obs.Validated()
		}
	}

	if err := deploy.Run(ctx, conn, pre, vars, scriptHook(obs, "pre")); err != nil {
		return out, err
	}
	out.Applied, err = execute.ApplyStream(ctx, conn, res.Statements, req.Exec, obs.Statement)
	if err != nil {
		return out, &ApplyError{Applied: out.Applied, Total: len(res.Statements), Err: err}
	}
	if err := deploy.Run(ctx, conn, post, vars, scriptHook(obs, "post")); err != nil {
		return out, err
	}
	// Record the resulting schema as the target's baseline for drift checks.
	if req.RecordBaseline && wholeDB {
		after, err := introspect.Introspect(ctx, conn, introspect.Options{})
		if err != nil {
			return out, &BaselineError{Applied: out.Applied, Err: fmt.Errorf("introspect target: %w", err)}
		}
		if out.Baseline, err = drift.Record(ctx, conn, after, req.Source, req.Generator); err != nil {
			return out, &BaselineError{Applied: out.Applied, Err: err}
		}
	}
	return out, nil
}

func scriptHook(obs Observer, phase string) func(string) {
	if obs.Script == nil {
		return nil
	}
	return func(name string) { obs.Script(phase, name) }
}

// IsDrift reports whether err is a drift refusal.
func IsDrift(err error) bool {
	var d *DriftError
	return errors.As(err, &d)
}

// ProjectRequest describes an update of a declarative project from a desired
// state: SSDT's "Update project from database", the reverse of a database
// publish.
type ProjectRequest struct {
	// Dir is the project directory being updated (the engine's "from") and
	// Manifest its loaded pgdac.yaml.
	Dir      string
	Manifest *project.Manifest
	// Source is the desired-state spec (the engine's "to").
	Source        string
	SourceOptions source.Options
	// Engine holds the comparison options; the project's own compare settings
	// and drop policy are the caller's defaults for them (see
	// deploy.WithProjectDefaults).
	Engine engine.Options
}

// ProjectResult is a planned project update; the caller applies Plan once it
// has decided what to do about its refusals.
type ProjectResult struct {
	Plan      *projsync.Plan
	Migration *engine.Result
	// From and To are the models compared.
	From, To *model.Database
	// FromWarnings and ToWarnings come from resolving the two sides.
	FromWarnings, ToWarnings []string
}

// Project plans the update of a declarative project from a desired state.
func Project(ctx context.Context, req ProjectRequest) (*ProjectResult, error) {
	toModel, toWarns, err := source.Resolve(ctx, req.Source, req.SourceOptions)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	fromModel, fromWarns, err := source.Resolve(ctx, req.Dir, req.SourceOptions)
	if err != nil {
		return nil, fmt.Errorf("target project: %w", err)
	}
	res, err := engine.Compare(fromModel, toModel, req.Engine)
	if err != nil {
		return nil, err
	}
	idx, err := projsync.Scan(req.Dir, req.Manifest, fromModel)
	if err != nil {
		return nil, fmt.Errorf("index project files: %w", err)
	}
	plan, err := projsync.Build(idx, req.Manifest, fromModel, toModel, res)
	if err != nil {
		return nil, err
	}
	return &ProjectResult{Plan: plan, Migration: res, From: fromModel, To: toModel, FromWarnings: fromWarns, ToWarnings: toWarns}, nil
}
