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
	// PartProject is the project's pgdac.yaml, when the artifact was built from
	// a project; PartPreDeployDir / PartPostDeployDir hold its deployment
	// scripts as numbered parts (NNNN_<file>.sql) in execution order.
	PartProject       = "project/pgdac.yaml"
	PartRefactorLog   = "project/refactorlog.json"
	PartPreDeployDir  = "deploy/pre/"
	PartPostDeployDir = "deploy/post/"
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

	// Deploy describes the deployment-time content carried next to the model
	// (drop policy, pre/post-deploy script parts), when any.
	Deploy *DeployInfo `json:"deploy,omitempty"`

	// Kind is "" or "pgdac" for a schema artifact and "pgbac" for a schema +
	// data archive (the BACPAC analog, see internal/pgbac); Data describes the
	// table data such an archive carries.
	Kind string    `json:"kind,omitempty"`
	Data *DataInfo `json:"data,omitempty"`

	// Parts lists the SHA-256 (hex) of every archived part except this manifest.
	Parts []Part `json:"parts"`
	// MerkleRoot is the hex Merkle root over the part digests.
	MerkleRoot string `json:"merkleRoot"`
	// Signature is present when the artifact was signed. Since format 3 it
	// covers the whole manifest (every field above), not just the Merkle root.
	Signature *Signature `json:"signature,omitempty"`
}

// DeployInfo is the manifest's record of a project's deployment-time content.
type DeployInfo struct {
	DropPolicy string   `json:"dropPolicy,omitempty"`
	PreDeploy  []string `json:"preDeploy,omitempty"`  // part names, in execution order
	PostDeploy []string `json:"postDeploy,omitempty"` // part names, in execution order
}

// DataInfo describes the table data a .pgbac carries next to its model: one
// gzip-compressed CSV part per table (generated columns excluded, since
// PostgreSQL recomputes them) and the position of every sequence, all read in
// one REPEATABLE READ transaction so the data is a consistent snapshot.
type DataInfo struct {
	Tables    []TableData     `json:"tables"`
	Sequences []SequenceState `json:"sequences,omitempty"`
	Snapshot  string          `json:"snapshot,omitempty"`
}

// TableData locates one table's rows in the archive.
type TableData struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Part    string   `json:"part"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
}

// SequenceState is a sequence's position (what pg_dump restores with setval).
type SequenceState struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	LastValue int64  `json:"lastValue"`
	IsCalled  bool   `json:"isCalled"`
}

// Part records the digest of one archived file.
type Part struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Signature is an Ed25519 signature. Format 3+: over the canonical JSON of the
// manifest with this field removed, so name, version, target, deploy info and
// the part digests are all covered. Formats 1–2 signed only the Merkle root.
type Signature struct {
	Algo string `json:"algo"` // "ed25519"
	// PublicKey is the base64-encoded signer public key, included so the artifact
	// is self-verifying for integrity; authenticity requires the verifier to also
	// trust this key (--require-key, or the trusted_keys file).
	PublicKey string `json:"publicKey"`
	// Sig is the base64-encoded signature.
	Sig string `json:"sig"`
}

// FormatVersion is the current .pgdac layout version.
//
//	1: initial layout.
//	2: canonical definitions are fully schema-qualified (introspection pins an
//	   empty search_path) and serial-backed sequences are modeled explicitly. A
//	   v1 artifact still opens, but comparing it against a v2 model can report
//	   qualification-only differences — rebuild it.
//	3: the signature covers the whole manifest (not only the Merkle root), and
//	   the artifact may carry the project's pgdac.yaml and pre/post-deploy
//	   scripts (manifest.deploy). Older artifacts verify with their own scheme.
const FormatVersion = 4
