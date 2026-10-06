// Package safety classifies the migration hazards of generated statements and
// encodes PostgreSQL's version-gated, lock-safety rules. The hazard taxonomy is
// adapted from Stripe's pg-schema-diff (MIT); the lock/online-DDL rules are
// reimplemented from the PostgreSQL documentation and the Squawk rule set
// (concepts only — no GPL code is copied).
package safety

// HazardType is a typed risk category attached to a generated statement so that
// callers (and the --allow-hazards gate) can reason about what a migration does.
type HazardType string

const (
	// AccessExclusiveLock: the statement takes an ACCESS EXCLUSIVE lock and will
	// queue ahead of all later queries on the table — even fast metadata-only
	// operations are dangerous behind a long-running reader.
	AccessExclusiveLock HazardType = "ACQUIRES_ACCESS_EXCLUSIVE_LOCK"
	// ShareRowExclusiveLock: e.g. ADD FOREIGN KEY (also locks the referenced table).
	ShareRowExclusiveLock HazardType = "ACQUIRES_SHARE_ROW_EXCLUSIVE_LOCK"
	// ShareLock: blocks writes but not reads.
	ShareLock HazardType = "ACQUIRES_SHARE_LOCK"
	// DeletesData: the statement can destroy data (the BlockOnPossibleDataLoss
	// analog). Gated behind --allow-hazards.
	DeletesData HazardType = "DELETES_DATA"
	// IndexBuild: builds an index (CPU/IO heavy; long-running).
	IndexBuild HazardType = "INDEX_BUILD"
	// IndexDropped: an index is removed (queries depending on it may regress).
	IndexDropped HazardType = "INDEX_DROPPED"
	// TableRewrite: rewrites the whole table and its indexes under ACCESS EXCLUSIVE.
	TableRewrite HazardType = "TABLE_REWRITE"
	// ImpactsPerformance: temporary performance impact during the operation.
	ImpactsPerformance HazardType = "IMPACTS_DATABASE_PERFORMANCE"
	// Correctness: the operation may not be semantically what the user intends.
	Correctness HazardType = "CORRECTNESS"
	// UpgradingExtension: ALTER EXTENSION ... UPDATE runs the extension's own scripts.
	UpgradingExtension HazardType = "UPGRADING_EXTENSION_VERSION"
	// Unsupported: the desired change cannot be applied in place by PostgreSQL
	// (dropping or reordering enum values, altering a type that is in use,
	// re-partitioning a table). The generated statement only raises a warning;
	// the schema will not converge until the change is made by hand. Error
	// severity, so a publish is blocked by default.
	Unsupported HazardType = "UNSUPPORTED_CHANGE"
	// MissingRole: a GRANT names a role the target cluster does not have. The
	// statement would fail, so the publish is blocked up front (error severity)
	// until the role exists or the grant is removed from the source.
	MissingRole HazardType = "MISSING_ROLE"
)

// Severity ranks how dangerous a hazard is, so callers can render badges and
// gate an apply (Error-severity hazards block publish unless explicitly allowed).
type Severity string

const (
	SeverityError   Severity = "error"   // can lose data / irreversible — blocks apply by default
	SeverityWarning Severity = "warning" // risky but routine (locks, rewrites, index builds)
	SeverityInfo    Severity = "info"
)

// Hazard is a single classified risk with a human-readable explanation.
type Hazard struct {
	Type     HazardType `json:"type"`
	Message  string     `json:"message"`
	Severity Severity   `json:"severity"`
}

// New constructs a hazard, assigning the default severity for its type.
func New(t HazardType, msg string) Hazard {
	return Hazard{Type: t, Message: msg, Severity: defaultSeverity(t)}
}

// defaultSeverity maps a hazard type to its default severity. Only data loss
// blocks an apply by default (the BlockOnPossibleDataLoss posture); everything
// else is a warning the user reviews.
func defaultSeverity(t HazardType) Severity {
	switch t {
	case DeletesData, Unsupported, MissingRole:
		return SeverityError
	default:
		return SeverityWarning
	}
}

// --- Version-gated capability checks (keyed on PostgreSQL major version) ---
//
// A major of 0 means "unknown"; callers should treat unknown as the most
// conservative path that still works on all supported versions (PG14+).

// MetadataOnlyAddColumn reports whether ADD COLUMN with a non-volatile default
// is metadata-only (no table rewrite). True since PG11.
func MetadataOnlyAddColumn(major int) bool { return major == 0 || major >= 11 }

// ScanFreeSetNotNull reports whether SET NOT NULL can skip the table scan when a
// validated CHECK (col IS NOT NULL) constraint exists. True since PG12.
func ScanFreeSetNotNull(major int) bool { return major == 0 || major >= 12 }

// ReindexConcurrently reports whether REINDEX CONCURRENTLY is available (PG12+).
func ReindexConcurrently(major int) bool { return major == 0 || major >= 12 }

// AddEnumValueInTransaction reports whether ALTER TYPE ... ADD VALUE may run
// inside a transaction block (PG12+; the value is just unusable until commit).
func AddEnumValueInTransaction(major int) bool { return major == 0 || major >= 12 }

// DetachPartitionConcurrently reports whether DETACH PARTITION CONCURRENTLY is
// available (PG14+).
func DetachPartitionConcurrently(major int) bool { return major == 0 || major >= 14 }

// VirtualGeneratedColumns reports whether VIRTUAL generated columns (no rewrite)
// are available (PG18+).
func VirtualGeneratedColumns(major int) bool { return major >= 18 }
