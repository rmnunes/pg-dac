package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rodrigonunes/pg-dac/internal/artifact"
	"github.com/rodrigonunes/pg-dac/internal/engine"
	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/project"
	"github.com/rodrigonunes/pg-dac/internal/source"
)

func newBuildCmd() *cobra.Command {
	return artifactWriteCmd(
		"build",
		"Build a signed .pgdac artifact from a source schema (database, project, or .sql file)",
	)
}

func newExtractCmd() *cobra.Command {
	return artifactWriteCmd(
		"extract",
		"Reverse-engineer a schema into a signed .pgdac artifact (alias of build)",
	)
}

func artifactWriteCmd(use, short string) *cobra.Command {
	var from, out, name, version, toProject string
	var schemas []string
	var targetMajor int
	var noSign bool

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if out == "" && toProject == "" {
				return fmt.Errorf("specify --out <file.pgdac> or --to-project <dir>")
			}
			m, warns, err := source.Resolve(ctx, from, source.Options{Schemas: schemas, TargetMajor: targetMajor})
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			printWarnings(cmd, "--from", warns)

			if toProject != "" {
				return writeProject(cmd, toProject, name, version, schemas, m)
			}

			if name == "" {
				name = strings.TrimSuffix(filepath.Base(out), ".pgdac")
			}
			opts := artifact.BuildOptions{
				Name:      name,
				Version:   version,
				Generator: "pg-dac " + appVersion(),
				SchemaSQL: canonicalSQL(m),
			}
			if !noSign {
				key, err := artifact.LoadOrCreateSigningKey()
				if err != nil {
					return fmt.Errorf("load signing key: %w", err)
				}
				opts.SigningKey = key
			}

			man, err := artifact.Build(out, m, opts)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Wrote %s (%s v%s, target PG %d, %d part(s))\n",
				out, man.Name, man.Version, man.TargetMajor, len(man.Parts))
			if man.Signature != nil {
				op := &artifact.Opened{Manifest: *man}
				fmt.Fprintf(w, "Signed (ed25519, key %s)\n", op.Fingerprint())
			} else {
				fmt.Fprintln(w, "Unsigned")
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&from, "from", "", "source schema: connection string, project directory, or .sql file")
	fl.StringVar(&out, "out", "", "output artifact path (e.g. app.pgdac)")
	fl.StringVar(&name, "name", "", "application name (default: the output filename)")
	fl.StringVar(&version, "version", "1.0.0", "application version")
	fl.StringArrayVar(&schemas, "schema", nil, "restrict to these schemas (repeatable)")
	fl.IntVar(&targetMajor, "target-major", 0, "PostgreSQL major version used to normalize a project/.sql source (0 = the project's pgdac.yaml, else 16)")
	fl.BoolVar(&noSign, "no-sign", false, "do not sign the artifact (integrity checksum only)")
	fl.StringVar(&toProject, "to-project", "", "reverse-engineer the source into a declarative project directory instead of a .pgdac")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}

// writeProject lays a model out as a one-object-per-file declarative project
// with a pgdac.yaml manifest (the SqlPackage ExtractTarget=SqlProject analog).
func writeProject(cmd *cobra.Command, dir, name, version string, schemas []string, m *model.Database) error {
	if name == "" {
		abs, _ := filepath.Abs(dir)
		name = filepath.Base(abs)
	}
	scripts := engine.ObjectScripts(m)
	written := 0
	for _, sc := range scripts {
		sectionDir := filepath.Join(dir, sc.Section)
		if err := os.MkdirAll(sectionDir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(sectionDir, sc.FileName())
		if err := os.WriteFile(path, []byte(sc.SQL+"\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		written++
	}

	man := project.Default(name)
	man.Version = version
	// Extracted projects are laid out as one numbered file per object in the
	// global dependency order a from-empty publish uses (see engine.ObjectScripts),
	// not in the section folders `init` scaffolds for hand authoring.
	man.Include = []string{engine.ExtractSection + "/**/*.sql"}
	if mv := m.MajorVersion(); mv > 0 {
		man.TargetMajor = mv
	}
	man.Schemas = schemas
	if err := man.Save(dir); err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Extracted %d script(s) into project %q at %s\n", written, name, dir)
	fmt.Fprintf(w, "  %s (targetMajor %d); files under %s/ apply in dependency order\n", project.ManifestName, man.TargetMajor, engine.ExtractSection)
	return nil
}

func newInspectCmd() *cobra.Command {
	var requireKey string
	cmd := &cobra.Command{
		Use:   "inspect <artifact.pgdac>",
		Short: "Verify a .pgdac artifact and print its manifest and contents",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := artifact.Open(args[0])
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			m := o.Manifest
			fmt.Fprintf(w, "Name:        %s\n", m.Name)
			fmt.Fprintf(w, "Version:     %s\n", m.Version)
			fmt.Fprintf(w, "Generator:   %s\n", m.Generator)
			fmt.Fprintf(w, "Target PG:   %d\n", m.TargetMajor)
			fmt.Fprintf(w, "Created:     %s\n", m.CreatedAt)
			fmt.Fprintf(w, "Merkle root: %s\n", m.MerkleRoot)
			fmt.Fprintf(w, "Integrity:   %s\n", okFailed(o.IntegrityOK))
			if o.Signed {
				fmt.Fprintf(w, "Signature:   %s (ed25519, key %s)\n", okFailed(o.SignatureOK), o.Fingerprint())
			} else {
				fmt.Fprintln(w, "Signature:   unsigned")
			}

			// Object summary.
			schemas, tables, views, seqs, enums := summarize(o.Model)
			fmt.Fprintf(w, "Contents:    %d schema(s), %d table(s), %d view(s), %d sequence(s), %d enum(s)\n",
				schemas, tables, views, seqs, enums)

			if !o.IntegrityOK {
				return fmt.Errorf("artifact integrity check FAILED (corrupt or tampered)")
			}
			if o.Signed && !o.SignatureOK {
				return fmt.Errorf("artifact signature verification FAILED")
			}
			if requireKey != "" {
				if !o.Signed {
					return fmt.Errorf("artifact is unsigned but --require-key was given")
				}
				if o.Fingerprint() != requireKey {
					return fmt.Errorf("artifact signed by key %s, not the required %s", o.Fingerprint(), requireKey)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&requireKey, "require-key", "", "fail unless the artifact is signed by this key fingerprint")
	return cmd
}

// canonicalSQL renders a from-empty CREATE script for the model, used as the
// human-readable sql/schema.sql part of the artifact. A baseline containing an
// empty public schema avoids emitting a spurious CREATE SCHEMA "public".
func canonicalSQL(m *model.Database) string {
	baseline := &model.Database{Schemas: []*model.Schema{{Name: "public"}}}
	res, err := engine.Compare(baseline, m, engine.Options{Online: false})
	if err != nil {
		return "-- (could not render canonical SQL: " + err.Error() + ")\n"
	}
	return res.Script(true)
}

func summarize(d *model.Database) (schemas, tables, views, seqs, enums int) {
	schemas = len(d.Schemas)
	for _, s := range d.Schemas {
		tables += len(s.Tables)
		views += len(s.Views) + len(s.MatViews)
		seqs += len(s.Sequences)
		enums += len(s.Enums)
	}
	return
}

func okFailed(ok bool) string {
	if ok {
		return "OK"
	}
	return "FAILED"
}

func appVersion() string { return version }
