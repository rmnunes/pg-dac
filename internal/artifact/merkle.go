package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// sha256Hex returns the hex-encoded SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// merkleRoot computes a Merkle root over a set of leaf digests. Leaves are sorted
// first so the root is independent of part ordering. With one leaf the root is
// that leaf; an odd node at any level is promoted unchanged to the next level.
// Returns the hex-encoded root, or the hash of empty input when there are no
// leaves.
func merkleRoot(leafHexes []string) string {
	if len(leafHexes) == 0 {
		return sha256Hex(nil)
	}
	level := make([][]byte, 0, len(leafHexes))
	for _, h := range leafHexes {
		b, err := hex.DecodeString(h)
		if err != nil {
			// A non-hex leaf can't occur from sha256Hex; hash the raw string to stay total.
			s := sha256.Sum256([]byte(h))
			b = s[:]
		}
		level = append(level, b)
	}
	sort.Slice(level, func(i, j int) bool { return bytes.Compare(level[i], level[j]) < 0 })

	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				pair := append(append([]byte{}, level[i]...), level[i+1]...)
				sum := sha256.Sum256(pair)
				next = append(next, sum[:])
			} else {
				next = append(next, level[i]) // promote odd node
			}
		}
		level = next
	}
	return hex.EncodeToString(level[0])
}
