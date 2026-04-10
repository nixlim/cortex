package infra

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// neo4jPasswordKey is the top-level YAML key under which the generated
// Neo4j bootstrap password is stored in ~/.cortex/config.yaml. The key
// is intentionally not part of the typed config.Config struct: the
// loader uses KnownFields(false) and silently ignores unknown keys, so
// we can round-trip the password through the same file without forcing
// every call site to acquire a typed field.
const neo4jPasswordKey = "neo4j_password"

// EnsureNeo4jPassword guarantees that the Cortex config file at path
// carries a non-empty neo4j_password key. If the key already has a
// value, that value is returned unchanged and generated is false. If
// the file does not exist or the key is missing, a cryptographically
// random 24-byte password is generated, merged into the existing YAML
// document, written with mode 0600, and returned with generated=true.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Credential management" (FR / US-10.7)
//   docs/spec/cortex-spec.md BDD scenario "First-time setup generates
//   Neo4j credentials".
//
// EnsureNeo4jPassword never logs or prints the password; callers that
// need to surface the fact that a new password was written should rely
// on the generated flag and point the operator at the config file.
func EnsureNeo4jPassword(path string) (password string, generated bool, err error) {
	raw, err := readRawConfig(path)
	if err != nil {
		return "", false, err
	}
	if v, ok := raw[neo4jPasswordKey].(string); ok && v != "" {
		return v, false, nil
	}
	pw, err := generatePassword(24)
	if err != nil {
		return "", false, fmt.Errorf("credentials: generate: %w", err)
	}
	raw[neo4jPasswordKey] = pw
	if err := writeRawConfig(path, raw); err != nil {
		return "", false, err
	}
	return pw, true, nil
}

// readRawConfig parses path as a top-level YAML mapping into a raw
// map[string]any so we can round-trip unknown keys. A missing file is
// treated as an empty mapping.
func readRawConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("credentials: read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("credentials: parse %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// writeRawConfig marshals m as YAML and writes it to path with
// owner-only permissions (0600). The parent directory is created
// (or, if already present, forcibly re-permissioned) with mode 0700.
// Writes are atomic: content is staged in a sibling tempfile and
// renamed into place.
//
// The explicit Chmod after MkdirAll is load-bearing for cortex-5r3:
// MkdirAll is a no-op when the directory already exists, so a
// ~/.cortex created by an older cortex release (or by a third-party
// tool) at mode 0755 would survive forever and trip the doctor's
// fs.permissions check on every run. Chmod'ing unconditionally makes
// writeRawConfig self-healing.
func writeRawConfig(path string, m map[string]any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credentials: ensure dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("credentials: chmod dir: %w", err)
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("credentials: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("credentials: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("credentials: rename: %w", err)
	}
	// Rename preserves the tmpfile mode on POSIX, but chmod defensively
	// in case the underlying FS stripped bits.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("credentials: chmod: %w", err)
	}
	return nil
}

func generatePassword(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	// RawURLEncoding avoids characters that would need YAML quoting.
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
