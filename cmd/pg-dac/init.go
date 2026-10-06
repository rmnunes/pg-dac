package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/project"
)

func newInitCmd() *cobra.Command {
	var dir, name string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a new pg-dac project (pgdac.yaml + ordered section folders)",
		Long: "Creates a declarative project: a pgdac.yaml manifest plus numbered section\n" +
			"folders (01-extensions … 09-policies) and pre-deploy/post-deploy directories.\n" +
			"Author one object per .sql file, then `pg-dac build --from <dir>` to produce a .pgdac.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				dir = "."
			}
			if name == "" {
				abs, _ := filepath.Abs(dir)
				name = filepath.Base(abs)
			}
			if project.Exists(dir) {
				return fmt.Errorf("%s already exists in %s", project.ManifestName, dir)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}

			dirs := append([]string{"pre-deploy", "post-deploy"}, project.DefaultSections...)
			for _, d := range dirs {
				full := filepath.Join(dir, d)
				if err := os.MkdirAll(full, 0o755); err != nil {
					return err
				}
				// .gitkeep so empty section folders survive version control.
				keep := filepath.Join(full, ".gitkeep")
				if err := os.WriteFile(keep, nil, 0o644); err != nil {
					return err
				}
			}

			m := project.Default(name)
			if err := m.Save(dir); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Initialized pg-dac project %q in %s\n", name, dir)
			fmt.Fprintf(w, "  %s (targetMajor %d)\n", project.ManifestName, m.TargetMajor)
			fmt.Fprintf(w, "  sections: %v\n", project.DefaultSections)
			fmt.Fprintln(w, "Author one object per .sql file under the section folders, then:")
			fmt.Fprintf(w, "  pg-dac build --from %s --out %s.pgdac\n", dir, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "directory to initialize the project in")
	cmd.Flags().StringVar(&name, "name", "", "project name (default: the directory name)")
	return cmd
}
