package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/deploy"
	"github.com/rmnunes/pg-dac/internal/project"
	"github.com/rmnunes/pg-dac/internal/publish"
)

// publishToProject updates a declarative project (--from) so it describes the
// desired state (--to): SSDT's "Update project from database", the reverse of
// a database publish. Only the files of objects the comparison reports are
// touched; the project's own compare settings and drop policy govern the
// update unless flags say otherwise; --dry-run prints the file diff.
func publishToProject(cmd *cobra.Command, f *commonFlags, dryRun bool) error {
	out := cmd.OutOrStdout()
	dir := strings.TrimPrefix(f.from, "file://")

	srcOpts, err := f.sourceOptions()
	if err != nil {
		return err
	}
	man, err := project.Load(dir)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	// The desired state's deployment content is irrelevant here; the project
	// being updated supplies the comparison defaults.
	dplan, err := f.loadDeployPlan(cmd)
	if err != nil {
		return err
	}
	deploy.WithProjectDefaults(dplan, man)
	f.dropPolicy = dplan.DropPolicy

	pr, err := publish.Project(cmd.Context(), publish.ProjectRequest{
		Dir: dir, Manifest: man, Source: f.to, SourceOptions: srcOpts, Engine: f.options(),
	})
	if err != nil {
		return err
	}
	printWarnings(cmd, "--from", pr.FromWarnings)
	printWarnings(cmd, "--to", pr.ToWarnings)
	printWarnings(cmd, "plan", pr.Migration.Warnings)

	plan := pr.Plan
	for _, r := range plan.Refused {
		fmt.Fprintf(cmd.ErrOrStderr(), "refused: %s — %s\n", r.Object, r.Reason)
	}
	if plan.Empty() {
		if len(plan.Refused) > 0 {
			return fmt.Errorf("nothing could be updated: %d object(s) are defined imperatively (see above)", len(plan.Refused))
		}
		fmt.Fprintln(out, "Project is already up to date.")
		return nil
	}
	fmt.Fprint(cmd.ErrOrStderr(), plan.Summary())
	if dryRun {
		fmt.Fprint(out, plan.Diff())
		return nil
	}
	if len(plan.Refused) > 0 && !f.allowHaz {
		return fmt.Errorf("refusing to update the project: %d object(s) are defined imperatively and cannot be rewritten in place (see above); re-run with --allow-hazards to update the others and edit those by hand", len(plan.Refused))
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %d file(s) in %s", len(plan.Changes), dir)
	if len(plan.RolesAdded) > 0 {
		fmt.Fprintf(out, " and added role(s) %s to %s", strings.Join(plan.RolesAdded, ", "), project.ManifestName)
	}
	fmt.Fprintln(out, ".")
	return nil
}
