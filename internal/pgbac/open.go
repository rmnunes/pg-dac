package pgbac

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rmnunes/pg-dac/internal/artifact"
	"github.com/rmnunes/pg-dac/internal/model"
)

// Opened is a .pgbac read for inspection or import. The manifest, model and
// other small parts are read and verified up front; data parts are only
// listed here and verified while streaming (see Import), so an archive of any
// size opens without loading its rows into memory.
type Opened struct {
	Path        string
	Manifest    artifact.Manifest
	Model       *model.Database
	IntegrityOK bool // small parts match their digests and the Merkle root recomputes over all recorded digests
	Signed      bool
	SignatureOK bool

	zr *zip.ReadCloser
}

// Open reads and verifies a .pgbac. Close it when done.
func Open(path string) (*Opened, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	o := &Opened{Path: path, zr: zr}
	files := map[string]*zip.File{}
	for _, zf := range zr.File {
		files[zf.Name] = zf
	}
	manFile, ok := files[artifact.PartManifest]
	if !ok {
		zr.Close()
		return nil, fmt.Errorf("archive is missing %s", artifact.PartManifest)
	}
	manBytes, err := readAll(manFile)
	if err != nil {
		zr.Close()
		return nil, err
	}
	if err := json.Unmarshal(manBytes, &o.Manifest); err != nil {
		zr.Close()
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if o.Manifest.Kind != Kind {
		zr.Close()
		return nil, fmt.Errorf("%s is a %q artifact, not a .pgbac (schema + data)", path, orPgdac(o.Manifest.Kind))
	}

	o.IntegrityOK = true
	var leaves []string
	for _, p := range o.Manifest.Parts {
		zf, ok := files[p.Name]
		if !ok {
			zr.Close()
			return nil, fmt.Errorf("archive is missing part %s named in the manifest", p.Name)
		}
		leaves = append(leaves, p.SHA256)
		if strings.HasPrefix(p.Name, DataDir) {
			continue // verified while streaming
		}
		data, err := readAll(zf)
		if err != nil {
			zr.Close()
			return nil, err
		}
		if artifact.SHA256Hex(data) != p.SHA256 {
			o.IntegrityOK = false
		}
		if p.Name == artifact.PartModel {
			if o.Model, err = model.Unmarshal(data); err != nil {
				zr.Close()
				return nil, fmt.Errorf("parse model: %w", err)
			}
		}
	}
	if artifact.MerkleRoot(leaves) != o.Manifest.MerkleRoot {
		o.IntegrityOK = false
	}
	if o.Model == nil {
		zr.Close()
		return nil, fmt.Errorf("archive is missing %s", artifact.PartModel)
	}
	o.Signed = o.Manifest.Signature != nil
	if o.Signed {
		o.SignatureOK = artifact.VerifyManifest(&o.Manifest) && o.IntegrityOK
	}
	return o, nil
}

// Close releases the archive.
func (o *Opened) Close() error { return o.zr.Close() }

// Fingerprint returns the signer key fingerprint, or "" if unsigned.
func (o *Opened) Fingerprint() string {
	a := &artifact.Opened{Manifest: o.Manifest}
	return a.Fingerprint()
}

// openPart returns a reader over one archived part.
func (o *Opened) openPart(name string) (io.ReadCloser, error) {
	for _, zf := range o.zr.File {
		if zf.Name == name {
			return zf.Open()
		}
	}
	return nil, fmt.Errorf("archive is missing part %s", name)
}

// verifyPart streams one part through SHA-256 and compares with the manifest.
func (o *Opened) verifyPart(name string) error {
	var want string
	for _, p := range o.Manifest.Parts {
		if p.Name == name {
			want = p.SHA256
		}
	}
	rc, err := o.openPart(name)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("archive integrity check failed: part %s does not match its recorded digest (corrupt or tampered); nothing from it was loaded", name)
	}
	return nil
}

func readAll(zf *zip.File) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", zf.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", zf.Name, err)
	}
	return data, nil
}

func orPgdac(kind string) string {
	if kind == "" {
		return "pgdac"
	}
	return kind
}
