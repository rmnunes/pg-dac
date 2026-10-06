package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/refactor"
)

// newRefactorCmd manages a project's refactorlog.json: the renames that
// publish applies as ALTER ... RENAME (once per target) instead of letting the
// diff drop and recreate the object.
func newRefactorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refactor",
		Short: "Record renames in the project's refactorlog so publish renames instead of drop + create",
		Long: "A declarative project cannot express a rename: the diff sees one object gone and\n" +
			"another appeared. refactorlog.json records the rename with a stable id; publish\n" +
			"applies it as ALTER ... RENAME (and remembers the id in the target's\n" +
			"pgdac.refactorlog) before diffing, so data and dependents are preserved.",
	}
	cmd.AddCommand(newRefactorRenameCmd(), newRefactorListCmd())
	return cmd
}

func newRefactorRenameCmd() *cobra.Command {
	var dir, kind, schema, table, from, to, toSchema, note string
	cmd := &cobra.Command{
		Use:   "rename",
		Short: "Append a rename to refactorlog.json",
		Example: "  pg-dac refactor rename --dir ./schema --kind table --schema app --from customers --to clients\n" +
			"  pg-dac refactor rename --dir ./schema --kind column --schema app --table clients --from name --to full_name\n" +
			"  pg-dac refactor rename --dir ./schema --kind function --schema app --from 'total(integer)' --to grand_total",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := refactor.Load(dir)
			if err != nil {
				return err
			}
			op, err := log.Add(refactor.Op{Kind: refactor.Kind(kind), Schema: schema, Table: table, From: from, To: to, ToSchema: toSchema, Note: note})
			if err != nil {
				return err
			}
			if err := log.Save(dir); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s (id %s) in %s/%s\n", op.Describe(), op.ID, dir, refactor.LogName)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&dir, "dir", ".", "project directory holding refactorlog.json")
	fl.StringVar(&kind, "kind", "", "object kind: table, column, view, matview, sequence, function, index, constraint, trigger, policy, enum, domain, compositeType, schema")
	fl.StringVar(&schema, "schema", "", "schema of the object (not for --kind schema)")
	fl.StringVar(&table, "table", "", "owning table/view (column, constraint, trigger, policy)")
	fl.StringVar(&from, "from", "", "current name (a function: its signature, e.g. 'f(integer)')")
	fl.StringVar(&to, "to", "", "new name")
	fl.StringVar(&toSchema, "to-schema", "", "also move the object to this schema")
	fl.StringVar(&note, "note", "", "free-text note")
	_ = cmd.MarkFlagRequired("kind")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func newRefactorListCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the renames recorded in refactorlog.json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := refactor.Load(dir)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(log.Operations) == 0 {
				fmt.Fprintln(w, "No refactorlog operations.")
				return nil
			}
			for _, op := range log.Operations {
				fmt.Fprintf(w, "%s  %s", op.ID, op.Describe())
				if op.Note != "" {
					fmt.Fprintf(w, "  — %s", op.Note)
				}
				fmt.Fprintln(w)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "project directory holding refactorlog.json")
	return cmd
}
