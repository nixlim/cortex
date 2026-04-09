package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrInsecurePermissions is returned when the config file exists but has
// permissions looser than 0600. Cortex refuses to read such files because
// ~/.cortex/ must be owner-only.
var ErrInsecurePermissions = errors.New("config: file permissions must be 0600")

// MalformedError wraps a YAML parse failure with the offending key name when
// the parser can identify it. The message always names a key so that the
// operator can locate the problem without reading a stack trace.
type MalformedError struct {
	Path string
	Key  string
	Err  error
}

func (e *MalformedError) Error() string {
	if e.Key != "" {
		return fmt.Sprintf("config: malformed YAML at %s (key %q): %v", e.Path, e.Key, e.Err)
	}
	return fmt.Sprintf("config: malformed YAML at %s: %v", e.Path, e.Err)
}

func (e *MalformedError) Unwrap() error { return e.Err }

// Load reads the config file at path and overlays it onto Defaults(). If the
// file does not exist, Load returns Defaults() with no error. If the file
// exists but has permissions other than 0600 (owner-only read/write), Load
// returns ErrInsecurePermissions without reading the file contents.
func Load(path string) (Config, error) {
	cfg := Defaults()

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: stat %s: %w", path, err)
	}

	// Verify permissions before reading. Spec: 0600 owner-only.
	mode := info.Mode().Perm()
	if mode != 0o600 {
		return cfg, fmt.Errorf("%w: got %#o on %s", ErrInsecurePermissions, mode, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}

	if len(data) == 0 {
		return cfg, nil
	}

	// Decode into the same Config struct so file values override defaults.
	dec := yaml.NewDecoder(bytesReader(data))
	dec.KnownFields(false)
	if err := dec.Decode(&cfg); err != nil {
		return Defaults(), &MalformedError{
			Path: path,
			Key:  extractKey(err, data),
			Err:  err,
		}
	}

	return cfg, nil
}

var lineRE = regexp.MustCompile(`line (\d+):`)

// extractKey parses a yaml.v3 error to locate the source line and returns
// the YAML key declared on that line (e.g. "decay_exponent" from
// "    decay_exponent: \"bad\""). If the key cannot be recovered, it falls
// back to the raw error message so diagnosis is still possible.
func extractKey(err error, source []byte) string {
	var te *yaml.TypeError
	if errors.As(err, &te) && len(te.Errors) > 0 {
		if k := keyFromLineMsg(te.Errors[0], source); k != "" {
			return k
		}
		return te.Errors[0]
	}
	if k := keyFromLineMsg(err.Error(), source); k != "" {
		return k
	}
	return err.Error()
}

func keyFromLineMsg(msg string, source []byte) string {
	m := lineRE.FindStringSubmatch(msg)
	if len(m) != 2 {
		return ""
	}
	lineno, err := strconv.Atoi(m[1])
	if err != nil || lineno <= 0 {
		return ""
	}
	lines := strings.Split(string(source), "\n")
	if lineno > len(lines) {
		return ""
	}
	line := strings.TrimSpace(lines[lineno-1])
	if i := strings.Index(line, ":"); i >= 0 {
		return strings.TrimSpace(line[:i])
	}
	return ""
}
