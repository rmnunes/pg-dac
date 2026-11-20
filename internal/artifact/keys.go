package artifact

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// keyFileName is the auto-managed signing key (the 32-byte Ed25519 seed,
// base64-encoded) under the user's config directory.
const keyFileName = "signing.key"

// keyDir returns the directory holding pg-dac's auto-managed signing key.
func keyDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "pg-dac"), nil
}

// LoadOrCreateSigningKey returns the user's Ed25519 signing key, generating and
// persisting one (0600) on first use so signing is zero-config.
func LoadOrCreateSigningKey() (ed25519.PrivateKey, error) {
	dir, err := keyDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, keyFileName)

	if data, err := os.ReadFile(path); err == nil {
		seed, err := base64.StdEncoding.DecodeString(string(data))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("malformed signing key at %s", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(seed)), 0o600); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// KeyFingerprint returns a short, human-comparable fingerprint of a public key
// (the first 8 bytes of its SHA-256, hex-encoded).
func KeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// trustedKeysFile lists the signer fingerprints an artifact must carry to be
// accepted as a source (one per line; blank lines and # comments ignored).
const trustedKeysFile = "trusted_keys"

// TrustedKeysPath returns the location of the trusted-keys file.
func TrustedKeysPath() (string, error) {
	dir, err := keyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, trustedKeysFile), nil
}

// TrustedKeysFor applies the signer-trust policy of every verb: an explicitly
// required key is the whole policy, otherwise the trusted_keys file (if any)
// decides which signers are accepted.
func TrustedKeysFor(requireKey string) ([]string, error) {
	if requireKey != "" {
		return nil, nil
	}
	keys, err := LoadTrustedKeys()
	if err != nil {
		return nil, fmt.Errorf("read trusted keys: %w", err)
	}
	return keys, nil
}

// LoadTrustedKeys reads the trusted-keys file. A missing file means "no trust
// list configured" (nil, nil): any signer is accepted with a warning.
func LoadTrustedKeys() ([]string, error) {
	path, err := TrustedKeysPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line != "" {
			keys = append(keys, line)
		}
	}
	return keys, nil
}
