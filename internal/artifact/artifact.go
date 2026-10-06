package artifact

import (
	"archive/zip"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rodrigonunes/pg-dac/internal/model"
)

// BuildOptions configures writing a .pgdac.
type BuildOptions struct {
	Name      string
	Version   string
	Generator string
	// SchemaSQL, if non-empty, is embedded as sql/schema.sql for human review.
	SchemaSQL string
	// CreatedAt is an RFC3339 timestamp; empty uses the current UTC time.
	CreatedAt string
	// SigningKey, if non-nil, signs the artifact (the whole manifest).
	SigningKey ed25519.PrivateKey
	// Extra parts to archive next to the model (project manifest, deploy
	// scripts); they are hashed, included in the Merkle root and thus signed.
	Extra []ExtraPart
	// Deploy is recorded in the manifest when the artifact carries deployment
	// content.
	Deploy *DeployInfo
}

// ExtraPart is an additional file to archive.
type ExtraPart struct {
	Name string
	Data []byte
}

// Build writes a .pgdac archive for model m at path and returns its manifest.
func Build(path string, m *model.Database, opts BuildOptions) (*Manifest, error) {
	modelBytes, err := m.Marshal()
	if err != nil {
		return nil, err
	}

	type part struct {
		name string
		data []byte
	}
	parts := []part{{PartModel, modelBytes}}
	if opts.SchemaSQL != "" {
		parts = append(parts, part{PartSchema, []byte(opts.SchemaSQL)})
	}
	for _, x := range opts.Extra {
		parts = append(parts, part{x.name(), x.Data})
	}

	var mparts []Part
	var leaves []string
	for _, p := range parts {
		h := sha256Hex(p.data)
		mparts = append(mparts, Part{Name: p.name, SHA256: h})
		leaves = append(leaves, h)
	}
	root := merkleRoot(leaves)

	createdAt := opts.CreatedAt
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339)
	}
	man := &Manifest{
		FormatVersion: FormatVersion,
		Name:          opts.Name,
		Version:       opts.Version,
		Generator:     opts.Generator,
		TargetMajor:   m.MajorVersion(),
		CreatedAt:     createdAt,
		Deploy:        opts.Deploy,
		Parts:         mparts,
		MerkleRoot:    root,
	}
	if opts.SigningKey != nil {
		pub := opts.SigningKey.Public().(ed25519.PublicKey)
		payload, err := signingPayload(man)
		if err != nil {
			return nil, err
		}
		sig := ed25519.Sign(opts.SigningKey, payload)
		man.Signature = &Signature{
			Algo:      "ed25519",
			PublicKey: base64.StdEncoding.EncodeToString(pub),
			Sig:       base64.StdEncoding.EncodeToString(sig),
		}
	}
	manBytes, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}

	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	writePart := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := writePart(PartManifest, manBytes); err != nil {
		return nil, err
	}
	for _, p := range parts {
		if err := writePart(p.name, p.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return man, nil
}

// Opened is the result of reading and verifying a .pgdac.
type Opened struct {
	Manifest    Manifest
	Model       *model.Database
	IntegrityOK bool // every part hash matched and the Merkle root recomputed
	Signed      bool
	SignatureOK bool // the signature verified (whole manifest for format 3+, Merkle root before)
	// Parts holds every archived file by name (including deploy scripts).
	Parts map[string][]byte
}

// Fingerprint returns the signer key fingerprint, or "" if unsigned.
func (o *Opened) Fingerprint() string {
	if o.Manifest.Signature == nil {
		return ""
	}
	pub, err := base64.StdEncoding.DecodeString(o.Manifest.Signature.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ""
	}
	return KeyFingerprint(pub)
}

// Open reads a .pgdac, verifies its integrity and signature, and parses the
// model. A returned error means the archive could not be read or is structurally
// invalid; integrity/signature outcomes are reported in the result, not as
// errors, so callers can decide how strict to be.
func Open(path string) (*Opened, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	defer zr.Close()

	files := map[string][]byte{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", zf.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", zf.Name, err)
		}
		files[zf.Name] = data
	}

	manBytes, ok := files[PartManifest]
	if !ok {
		return nil, fmt.Errorf("artifact is missing %s", PartManifest)
	}
	var man Manifest
	if err := json.Unmarshal(manBytes, &man); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	integrityOK := true
	var leaves []string
	for _, p := range man.Parts {
		data, ok := files[p.Name]
		if !ok {
			return nil, fmt.Errorf("artifact is missing part %s named in the manifest", p.Name)
		}
		h := sha256Hex(data)
		if h != p.SHA256 {
			integrityOK = false
		}
		leaves = append(leaves, h)
	}
	if merkleRoot(leaves) != man.MerkleRoot {
		integrityOK = false
	}

	out := &Opened{Manifest: man, IntegrityOK: integrityOK, Signed: man.Signature != nil, Parts: files}
	if out.Signed {
		out.SignatureOK = verifySignature(&man) && integrityOK
	}

	modelBytes, ok := files[PartModel]
	if !ok {
		return nil, fmt.Errorf("artifact is missing %s", PartModel)
	}
	m, err := model.Unmarshal(modelBytes)
	if err != nil {
		return nil, fmt.Errorf("parse model: %w", err)
	}
	out.Model = m
	return out, nil
}

// signingPayload is the canonical byte string a format-3 signature covers: the
// manifest serialized as JSON with the signature field removed. Every field
// (name, version, target, created-at, deploy info, part digests, Merkle root)
// is therefore authenticated, not just the content hashes.
func signingPayload(man *Manifest) ([]byte, error) {
	cp := *man
	cp.Signature = nil
	return json.Marshal(&cp)
}

func verifySignature(man *Manifest) bool {
	sig := man.Signature
	if sig == nil || sig.Algo != "ed25519" {
		return false
	}
	pub, err := base64.StdEncoding.DecodeString(sig.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return false
	}
	var payload []byte
	if man.FormatVersion >= 3 {
		if payload, err = signingPayload(man); err != nil {
			return false
		}
	} else {
		payload = []byte(man.MerkleRoot) // formats 1–2 signed only the Merkle root
	}
	return ed25519.Verify(ed25519.PublicKey(pub), payload, raw)
}

// name returns the part's archive path, normalized to forward slashes.
func (x ExtraPart) name() string { return strings.ReplaceAll(x.Name, "\\", "/") }
