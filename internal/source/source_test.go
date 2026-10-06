package source_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/source"
)

func buildArtifact(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	m := &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "public"}}}
	path := filepath.Join(t.TempDir(), "a.pgdac")
	if _, err := artifact.Build(path, m, artifact.BuildOptions{Name: "a", Version: "1", Generator: "test", SigningKey: key}); err != nil {
		t.Fatal(err)
	}
	return path
}

// Signer trust: --require-key and the trusted-keys list gate .pgdac sources;
// without either, the signer is reported, never silently accepted.
func TestResolveArtifactTrust(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fp := artifact.KeyFingerprint(pub)
	signed := buildArtifact(t, priv)
	unsigned := buildArtifact(t, nil)
	ctx := context.Background()

	if _, _, err := source.Resolve(ctx, signed, source.Options{RequireKey: fp}); err != nil {
		t.Errorf("required key matches: %v", err)
	}
	if _, _, err := source.Resolve(ctx, signed, source.Options{RequireKey: "deadbeefdeadbeef"}); err == nil || !strings.Contains(err.Error(), "not the required") {
		t.Errorf("wrong required key should fail, got %v", err)
	}
	if _, _, err := source.Resolve(ctx, signed, source.Options{TrustedKeys: []string{"deadbeefdeadbeef"}}); err == nil || !strings.Contains(err.Error(), "trusted-keys") {
		t.Errorf("untrusted signer should fail, got %v", err)
	}
	if _, warns, err := source.Resolve(ctx, signed, source.Options{TrustedKeys: []string{"other", fp}}); err != nil || len(warns) != 0 {
		t.Errorf("trusted signer: err=%v warns=%v", err, warns)
	}
	if _, warns, err := source.Resolve(ctx, signed, source.Options{}); err != nil || len(warns) != 1 || !strings.Contains(warns[0], fp) {
		t.Errorf("no trust configured: should accept and name the signer; err=%v warns=%v", err, warns)
	}
	if _, _, err := source.Resolve(ctx, unsigned, source.Options{RequireKey: fp}); err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Errorf("unsigned with required key should fail, got %v", err)
	}
	if _, _, err := source.Resolve(ctx, unsigned, source.Options{TrustedKeys: []string{fp}}); err == nil {
		t.Error("unsigned with a trust list should fail")
	}
	if _, warns, err := source.Resolve(ctx, unsigned, source.Options{}); err != nil || len(warns) != 1 || !strings.Contains(warns[0], "unsigned") {
		t.Errorf("unsigned, no trust configured: should accept with a warning; err=%v warns=%v", err, warns)
	}
}
