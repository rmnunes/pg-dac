package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/pgbac"
)

// newExportCmd writes a .pgbac: the schema and the data of a database in one
// consistent snapshot (SqlPackage Export).
func newExportCmd() *cobra.Command {
	var from, out, name, version string
	var schemas, tables, excludeTables []string
	var targetMajor int
	var noSign bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a database's schema AND data into a signed .pgbac archive (the BACPAC analog)",
		Long: "Reads the schema and every table's rows in one REPEATABLE READ transaction and writes them as a\n" +
			"portable, integrity-checked, signed archive. Import it into an empty database with `pg-dac import`.\n" +
			"--table / --exclude-table select which tables' data is carried (schema.table globs); the schema\n" +
			"is always complete.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(out), ".pgbac")
			}
			opts := pgbac.ExportOptions{
				Name: name, Version: version, Generator: "pg-dac " + appVersion(),
				Schemas: schemas, Tables: tables, ExcludeTables: excludeTables, TargetMajor: targetMajor,
				SchemaSQL: canonicalSQL,
				Progress: func(table string, rows int64) {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s: %d row(s)\n", table, rows)
				},
			}
			if !noSign {
				key, err := artifact.LoadOrCreateSigningKey()
				if err != nil {
					return fmt.Errorf("load signing key: %w", err)
				}
				opts.SigningKey = key
			}
			man, warns, err := pgbac.Export(cmd.Context(), from, out, opts)
			if err != nil {
				return err
			}
			printWarnings(cmd, "--from", warns)
			var rows int64
			for _, t := range man.Data.Tables {
				rows += t.Rows
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Wrote %s (%s v%s, target PG %d): %d table(s), %d row(s), %d sequence(s)\n",
				out, man.Name, man.Version, man.TargetMajor, len(man.Data.Tables), rows, len(man.Data.Sequences))
			if man.Signature != nil {
				fmt.Fprintf(w, "Signed (ed25519, key %s)\n", (&artifact.Opened{Manifest: *man}).Fingerprint())
			} else {
				fmt.Fprintln(w, "Unsigned")
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&from, "from", "", "source database connection string")
	fl.StringVar(&out, "out", "", "output archive path (e.g. app.pgbac)")
	fl.StringVar(&name, "name", "", "application name (default: the output filename)")
	fl.StringVar(&version, "version", "1.0.0", "application version")
	fl.StringArrayVar(&schemas, "schema", nil, "restrict to these schemas (repeatable)")
	fl.StringArrayVar(&tables, "table", nil, "export data only for tables matching this schema.table glob (repeatable)")
	fl.StringArrayVar(&excludeTables, "exclude-table", nil, "skip data for tables matching this schema.table glob (repeatable)")
	fl.IntVar(&targetMajor, "target-major", 0, "PostgreSQL major used to canonicalize the schema (0 = the source's)")
	fl.BoolVar(&noSign, "no-sign", false, "do not sign the archive (integrity checksum only)")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

// inspectPgbac prints a .pgbac's manifest, verification outcome and data summary.
func inspectPgbac(cmd *cobra.Command, path, requireKey string) error {
	o, err := pgbac.Open(path)
	if err != nil {
		return err
	}
	defer o.Close()
	w := cmd.OutOrStdout()
	m := o.Manifest
	fmt.Fprintf(w, "Kind:        pgbac (schema + data)\n")
	fmt.Fprintf(w, "Name:        %s\n", m.Name)
	fmt.Fprintf(w, "Version:     %s\n", m.Version)
	fmt.Fprintf(w, "Generator:   %s\n", m.Generator)
	fmt.Fprintf(w, "Target PG:   %d\n", m.TargetMajor)
	fmt.Fprintf(w, "Snapshot:    %s\n", m.CreatedAt)
	fmt.Fprintf(w, "Merkle root: %s\n", m.MerkleRoot)
	fmt.Fprintf(w, "Integrity:   %s (data parts are verified while importing)\n", okFailed(o.IntegrityOK))
	if o.Signed {
		fmt.Fprintf(w, "Signature:   %s (ed25519, key %s)\n", okFailed(o.SignatureOK), o.Fingerprint())
	} else {
		fmt.Fprintln(w, "Signature:   unsigned")
	}
	schemas, tables, views, seqs, enums := summarize(o.Model)
	fmt.Fprintf(w, "Schema:      %d schema(s), %d table(s), %d view(s), %d sequence(s), %d enum(s)\n", schemas, tables, views, seqs, enums)
	if m.Data != nil {
		var rows int64
		for _, t := range m.Data.Tables {
			rows += t.Rows
		}
		fmt.Fprintf(w, "Data:        %d table(s), %d row(s), %d sequence position(s)\n", len(m.Data.Tables), rows, len(m.Data.Sequences))
		for _, t := range m.Data.Tables {
			fmt.Fprintf(w, "  %s.%s: %d row(s)\n", t.Schema, t.Name, t.Rows)
		}
	}
	if !o.IntegrityOK {
		return fmt.Errorf("archive integrity check FAILED (corrupt or tampered)")
	}
	if o.Signed && !o.SignatureOK {
		return fmt.Errorf("archive signature verification FAILED")
	}
	if requireKey != "" {
		if _, err := artifact.Trust(path, o.Fingerprint(), o.Signed, requireKey, nil); err != nil {
			return err
		}
	}
	return nil
}

// newImportCmd recreates a .pgbac in an empty database (SqlPackage Import).
func newImportCmd() *cobra.Command {
	var file, target, requireKey string
	var noBaseline bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a .pgbac (schema and data) into an EMPTY database",
		Long: "Verifies the archive, creates the schema (pre-data objects first), loads every table with COPY,\n" +
			"restores sequence positions, then adds keys, indexes, triggers and policies and fills\n" +
			"materialized views — pg_dump's order. The target must not already hold the archive's schemas;\n" +
			"roles the archive grants to must exist. Records the result as the target's drift baseline.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			keys, err := artifact.TrustedKeysFor(requireKey)
			if err != nil {
				return err
			}
			opts := pgbac.ImportOptions{RequireKey: requireKey, TrustedKeys: keys, NoBaseline: noBaseline, Generator: "pg-dac " + appVersion(),
				Progress: func(msg string) { fmt.Fprintln(cmd.ErrOrStderr(), msg) }}
			rep, err := pgbac.Import(cmd.Context(), target, file, opts)
			if err != nil {
				return err
			}
			printWarnings(cmd, "--file", rep.Warnings)
			fmt.Fprintf(cmd.OutOrStdout(), "Imported %s: %d statement(s), %d table(s), %d row(s)", file, rep.Statements, rep.Tables, rep.Rows)
			if !noBaseline {
				fmt.Fprint(cmd.OutOrStdout(), "; baseline recorded")
			}
			fmt.Fprintln(cmd.OutOrStdout(), ".")
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&file, "file", "", "the .pgbac archive to import")
	fl.StringVar(&target, "target", "", "connection string of the empty database to fill")
	fl.StringVar(&requireKey, "require-key", "", "fail unless the archive is signed by this key fingerprint (default: the trusted_keys file, if any)")
	fl.BoolVar(&noBaseline, "no-baseline", false, "do not record the imported schema as the target's drift baseline")
	_ = cmd.MarkFlagRequired("file")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}
