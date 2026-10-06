package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The GUI bundles a version-pinned pg-dac sidecar. The pin (ui/PGDAC_VERSION),
// the frontend package version, the Tauri app version and the Rust crate
// version must agree; the release workflow stamps the same value into the Go
// binary (-X main.version) and checks the git tag against it.
func TestVersionsAgree(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(data)
	}
	pin := strings.TrimSpace(read("ui/PGDAC_VERSION"))
	if pin == "" {
		t.Fatal("ui/PGDAC_VERSION is empty")
	}

	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(read("ui/package.json")), &pkg); err != nil {
		t.Fatal(err)
	}
	var tauri struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(read("ui/src-tauri/tauri.conf.json")), &tauri); err != nil {
		t.Fatal(err)
	}
	cargo := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`).FindStringSubmatch(read("ui/src-tauri/Cargo.toml"))
	if cargo == nil {
		t.Fatal("no version in ui/src-tauri/Cargo.toml")
	}

	for name, got := range map[string]string{"ui/package.json": pkg.Version, "ui/src-tauri/tauri.conf.json": tauri.Version, "ui/src-tauri/Cargo.toml": cargo[1]} {
		if got != pin {
			t.Errorf("%s version %q does not match ui/PGDAC_VERSION %q", name, got, pin)
		}
	}
	// The default dev version is the pin plus a -dev suffix.
	if strings.TrimSuffix(version, "-dev") != pin {
		t.Errorf("main.version default %q should be %q(-dev)", version, pin)
	}
}
