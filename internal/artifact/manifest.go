// Package artifact reads and writes the .pgdac package: a portable, integrity-
// checked, optionally signed archive of a schema model. It is pg-dac's analog of
// a DACPAC — a single file you can hand around, compare against a database or
// another artifact, and (with a known public key) verify was not tampered with.
//
// Layout (a ZIP archive):
//
//	manifest.json    metadata + integrity block (+ optional signature)
//	model.json       the canonical schema model (see internal/model)
//	sql/schema.sql   canonical CREATE script, for human review (informational)
//
// Integrity: every part except manifest.json is SHA-256 hashed; the hashes form
// a Merkle root recorded in the manifest. A signature (Ed25519) over that root
// makes the artifact tamper-resistant for anyone who knows the signer's public
// key — an improvement over DACPAC's checksum-only (tamper-evident) seal.
//
// SECURITY NOTE: a .pgdac is compressed, not encrypted. Do not place secrets in
// schema defaults or comments expecting confidentiality.
package artifact

// FileName constants for the parts inside the archive.
const (
	PartManifest = "manifest.json"
	PartModel    = "model.json"
	PartSchema   = "sql/schema.sql"
)

// Manifest is the metadata and integrity block stored as manifest.json.
type Manifest struct {
	// FormatVersion identifies the .pgdac layout for forward compatibility.
	FormatVersion int `json:"formatVersion"`
	// Name and Version identify the data-tier application this artifact captures.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Generator is the pg-dac version that produced the artifact.
	Generator string `json:"generator"`
	// TargetMajor is the PostgreSQL major version the model targets, so version-
	// gated migration strategies are enforced by the package itself.
	TargetMajor int `json:"targetMajor"`
	// CreatedAt is an RFC3339 UTC timestamp.
	CreatedAt string `json:"createdAt"`

	// Parts lists the SHA-256 (hex) of every archived part except this manifest.
	Parts []Part `json:"parts"`
	// MerkleRoot is the hex Merkle root over the part digests.
	MerkleRoot string `json:"merkleRoot"`
	// Signature is present when the artifact was signed.
	Signature *Signature `json:"signature,omitempty"`
}

// Part records the digest of one archived file.
type Part struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Signature is an Ed25519 signature over the Merkle root.
type Signature struct {
	Algo string `json:"algo"` // "ed25519"
	// PublicKey is the base64-encoded signer public key, included so the artifact
	// is self-verifying for integrity; authenticity requires the verifier to also
	// trust this key (e.g. compare its fingerprint against a known value).
	PublicKey string `json:"publicKey"`
	// Sig is the base64-encoded signature over the Merkle-root bytes.
	Sig string `json:"sig"`
}

// FormatVersion is the current .pgdac layout version.
//
//	1: initial layout.
//	2: canonical definitions are fully schema-qualified (introspection pins an
//	   empty search_path) and serial-backed sequences are modeled explicitly. A
//	   v1 artifact still opens, but comparing it against a v2 model can report
//	   qualification-only differences — rebuild it.
const FormatVersion = 2
