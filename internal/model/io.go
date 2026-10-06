package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Marshal serializes the database model to deterministic, indented JSON. It
// sorts all collections first so that the output (and therefore the artifact's
// model.json and its checksum) is stable for identical schemas.
func (d *Database) Marshal() ([]byte, error) {
	d.Sort()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("marshal model: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal parses a database model from JSON produced by Marshal.
func Unmarshal(data []byte) (*Database, error) {
	var d Database
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("unmarshal model: %w", err)
	}
	return &d, nil
}
