package artifact

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"
)

// The format-3 signature covers the whole manifest: tampering with the name,
// version or target major must break it even though every part hash still
// matches.
func TestSignatureCoversManifestMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pgdac")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	post := PartPostDeployDir + "0001_seed.sql"
	if _, err := Build(path, sampleModel(), BuildOptions{
		Name: "app", Version: "1.0.0", Generator: "test", SigningKey: priv,
		Extra:  []ExtraPart{{Name: PartProject, Data: []byte("name: app\n")}, {Name: post, Data: []byte("SELECT 1;")}},
		Deploy: &DeployInfo{DropPolicy: "safe", PostDeploy: []string{post}},
	}); err != nil {
		t.Fatal(err)
	}

	o, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !o.IntegrityOK || !o.SignatureOK {
		t.Fatalf("fresh artifact: integrity=%v signature=%v", o.IntegrityOK, o.SignatureOK)
	}
	if o.Manifest.FormatVersion != FormatVersion || o.Manifest.Deploy == nil || o.Manifest.Deploy.DropPolicy != "safe" {
		t.Errorf("manifest: %+v", o.Manifest)
	}
	if string(o.Parts[post]) != "SELECT 1;" || string(o.Parts[PartProject]) != "name: app\n" {
		t.Errorf("deploy parts did not round-trip: %q %q", o.Parts[post], o.Parts[PartProject])
	}

	parts := readZip(t, path)
	var man Manifest
	if err := json.Unmarshal(parts[PartManifest], &man); err != nil {
		t.Fatal(err)
	}
	man.Name, man.Version, man.TargetMajor = "evil", "9.9.9", 99
	man.Deploy.DropPolicy = "all"
	parts[PartManifest], _ = json.MarshalIndent(man, "", "  ")
	tampered := filepath.Join(t.TempDir(), "tampered.pgdac")
	writeZip(t, tampered, parts)

	o, err = Open(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !o.IntegrityOK {
		t.Error("part hashes are untouched, integrity should still be OK")
	}
	if o.SignatureOK {
		t.Error("metadata tampering must break the signature")
	}

	// Tampering with a deploy script breaks integrity (and so the signature).
	parts = readZip(t, path)
	parts[post] = []byte("DROP TABLE everything;")
	tampered2 := filepath.Join(t.TempDir(), "tampered2.pgdac")
	writeZip(t, tampered2, parts)
	o, _ = Open(tampered2)
	if o.IntegrityOK || o.SignatureOK {
		t.Error("a modified deploy script must fail integrity and signature")
	}
}
