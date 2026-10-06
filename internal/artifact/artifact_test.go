package artifact

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

func sampleModel() *model.Database {
	return &model.Database{
		TargetVersion: 160004,
		Schemas: []*model.Schema{
			{Name: "public"},
			{Name: "app", Tables: []*model.Table{{
				Schema: "app", Name: "t",
				Columns: []*model.Column{{Name: "id", Type: "bigint", NotNull: true}},
			}}},
		},
	}
}

func TestBuildOpenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pgdac")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(path, sampleModel(), BuildOptions{
		Name: "app", Version: "1.0.0", Generator: "test", SchemaSQL: "-- demo schema", SigningKey: priv,
	}); err != nil {
		t.Fatalf("build: %v", err)
	}

	o, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !o.IntegrityOK {
		t.Error("integrity should be OK for a freshly built artifact")
	}
	if !o.Signed || !o.SignatureOK {
		t.Error("signature should verify for a freshly built signed artifact")
	}
	if o.Fingerprint() == "" {
		t.Error("expected a non-empty signer fingerprint")
	}
	if o.Manifest.Name != "app" || o.Manifest.Version != "1.0.0" || o.Manifest.TargetMajor != 16 {
		t.Errorf("unexpected manifest: %+v", o.Manifest)
	}
	// model round-trips
	var found bool
	for _, s := range o.Model.Schemas {
		if s.Name == "app" && len(s.Tables) == 1 && s.Tables[0].Name == "t" {
			found = true
		}
	}
	if !found {
		t.Error("model did not round-trip through the artifact")
	}
}

func TestTamperDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pgdac")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Build(path, sampleModel(), BuildOptions{Name: "app", Version: "1", Generator: "test", SigningKey: priv}); err != nil {
		t.Fatal(err)
	}

	parts := readZip(t, path)
	// Append JSON-insignificant whitespace: the model still parses, but the part
	// hash (and therefore the Merkle root and signature) no longer matches.
	parts[PartModel] = append(parts[PartModel], ' ', '\n')
	tampered := filepath.Join(t.TempDir(), "tampered.pgdac")
	writeZip(t, tampered, parts)

	o, err := Open(tampered)
	if err != nil {
		t.Fatalf("open tampered: %v", err)
	}
	if o.IntegrityOK {
		t.Error("tampered part must fail integrity")
	}
	if o.SignatureOK {
		t.Error("tampered part must fail signature verification")
	}
}

func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[zf.Name] = b
	}
	return out
}

func writeZip(t *testing.T, path string, parts map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range parts {
		w, _ := zw.Create(name)
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
