package diff

import "github.com/rmnunes/pg-dac/internal/model"

// Diff runs DiffWith under the default options, the way most of the differ's
// tests exercise it.
func Diff(from, to *model.Database) *ChangeSet { return DiffWith(from, to, Options{}) }
