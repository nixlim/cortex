package frames

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestLoadBuiltinHasExactly11Types(t *testing.T) {
	r, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	if r.Len() != 11 {
		t.Fatalf("expected 11 built-ins, got %d", r.Len())
	}
	want := append([]string(nil), BuiltinNames...)
	sort.Strings(want)

	got := make([]string, 0, 11)
	for _, n := range BuiltinNames {
		if _, ok := r.Get(n); !ok {
			t.Errorf("missing built-in %q", n)
		}
		got = append(got, n)
	}
	sort.Strings(got)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name mismatch: got %q want %q", got[i], want[i])
		}
	}
}

func TestBuiltinSchemaShapes(t *testing.T) {
	r, err := LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	// Observation: episodic, body required.
	obs, _ := r.Get("Observation")
	if obs.Store != StoreEpisodic || len(obs.Required) != 1 || obs.Required[0] != "body" || obs.ReflectionOnly {
		t.Errorf("Observation schema wrong: %+v", obs)
	}
	// RetryPattern: procedural, 6 required slots, reflection-only.
	rp, _ := r.Get("RetryPattern")
	if rp.Store != StoreProcedural || !rp.ReflectionOnly {
		t.Errorf("RetryPattern schema wrong: %+v", rp)
	}
	if len(rp.Required) != 6 {
		t.Errorf("RetryPattern required slots: got %d want 6", len(rp.Required))
	}
	// Every built-in has version >= 1.
	for _, n := range BuiltinNames {
		s, _ := r.Get(n)
		if s.Version < 1 {
			t.Errorf("%s version missing", n)
		}
	}
}

func TestCheckObserveKind(t *testing.T) {
	r, err := LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	// Allowed kinds.
	for _, n := range []string{"Observation", "SessionReflection", "ObservedRace"} {
		if err := r.CheckObserveKind(n); err != nil {
			t.Errorf("CheckObserveKind(%q) = %v; want nil", n, err)
		}
	}
	// Reflection-only kinds.
	for _, n := range []string{"BugPattern", "DesignDecision", "RetryPattern", "ReliabilityPattern", "SecurityPattern", "LibraryBehavior", "Principle", "ArchitectureNote"} {
		err := r.CheckObserveKind(n)
		if !errors.Is(err, ErrReflectionOnly) {
			t.Errorf("CheckObserveKind(%q) = %v; want ErrReflectionOnly", n, err)
		}
		if err == nil || !containsString(err.Error(), "REFLECTION_ONLY_KIND") {
			t.Errorf("CheckObserveKind(%q) error must mention REFLECTION_ONLY_KIND: %v", n, err)
		}
	}
	// Unknown.
	if err := r.CheckObserveKind("NotAKind"); err == nil || !containsString(err.Error(), "UNKNOWN_KIND") {
		t.Errorf("unknown kind error should contain UNKNOWN_KIND: %v", err)
	}
}

func TestCustomFrameRedefiningBuiltinRejected(t *testing.T) {
	dir := t.TempDir()
	// Write a custom file that tries to redefine BugPattern.
	bad := `{"name":"BugPattern","store":"semantic","required":["x"],"version":1}`
	if err := os.WriteFile(filepath.Join(dir, "evil.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadWithCustomDir(dir)
	if !errors.Is(err, ErrBuiltinRedefined) {
		t.Fatalf("expected ErrBuiltinRedefined, got %v", err)
	}
}

func TestCustomFrameValid(t *testing.T) {
	dir := t.TempDir()
	good := `{"name":"TeamConvention","store":"semantic","required":["name","body"],"version":1}`
	if err := os.WriteFile(filepath.Join(dir, "conv.json"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadWithCustomDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("TeamConvention"); !ok {
		t.Error("custom frame not loaded")
	}
	if r.Len() != 12 {
		t.Errorf("expected 12 frames (11 builtin + 1 custom), got %d", r.Len())
	}
	if r.IsBuiltin("TeamConvention") {
		t.Error("custom frame should not be flagged as builtin")
	}
}

func TestLoadMissingCustomDirOK(t *testing.T) {
	_, err := LoadWithCustomDir("/nonexistent/does/not/exist")
	if err != nil {
		t.Errorf("missing custom dir should be tolerated: %v", err)
	}
}

func containsString(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
