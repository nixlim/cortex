// Package frames embeds the Phase 1 built-in frame type registry and
// validates operator custom frames loaded from ~/.cortex/frames/*.json at
// startup.
//
// There are exactly 11 built-in types, split into three that agents may
// write directly via `cortex observe` (Observation, SessionReflection,
// ObservedRace) and eight that are reflection-only (BugPattern,
// DesignDecision, RetryPattern, ReliabilityPattern, SecurityPattern,
// LibraryBehavior, Principle, ArchitectureNote).
//
// Operators may add custom frame types by dropping JSON files into
// ~/.cortex/frames/. A custom file that redefines a built-in name is
// rejected with ErrBuiltinRedefined — operators cannot override built-ins
// in Phase 1.
package frames

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Store is the logical store a frame lives in.
type Store string

const (
	StoreEpisodic   Store = "episodic"
	StoreSemantic   Store = "semantic"
	StoreProcedural Store = "procedural"
)

// FrameSchema is the on-disk and in-memory shape of a frame type.
type FrameSchema struct {
	Name           string   `json:"name"`
	Store          Store    `json:"store"`
	Required       []string `json:"required"`
	Optional       []string `json:"optional"`
	ReflectionOnly bool     `json:"reflection_only"`
	Version        int      `json:"version"`
}

// Errors surfaced by the registry.
var (
	ErrBuiltinRedefined = errors.New("frames: custom file redefines a built-in frame")
	// ReflectionOnlyError is the sentinel for rejecting `cortex observe`
	// attempts to write a reflection-only frame type directly. The error
	// code is REFLECTION_ONLY_KIND per the acceptance criterion.
	ErrReflectionOnly = errors.New("REFLECTION_ONLY_KIND")
)

//go:embed builtin/*.json
var builtinFS embed.FS

// BuiltinNames lists the exact 11 Phase 1 built-in frame names in the
// canonical order specified in cortex-spec.md's Frame Type Registry table.
var BuiltinNames = []string{
	"Observation",
	"SessionReflection",
	"ObservedRace",
	"BugPattern",
	"DesignDecision",
	"RetryPattern",
	"ReliabilityPattern",
	"SecurityPattern",
	"LibraryBehavior",
	"Principle",
	"ArchitectureNote",
}

// Registry holds the loaded frame schemas.
type Registry struct {
	byName  map[string]FrameSchema
	builtin map[string]struct{}
}

// LoadBuiltin returns a registry populated from the embedded built-in
// frame files only. It never touches the filesystem.
func LoadBuiltin() (*Registry, error) {
	r := &Registry{
		byName:  map[string]FrameSchema{},
		builtin: map[string]struct{}{},
	}
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("frames: read embedded builtin dir: %w", err)
	}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		data, err := builtinFS.ReadFile("builtin/" + ent.Name())
		if err != nil {
			return nil, fmt.Errorf("frames: read %s: %w", ent.Name(), err)
		}
		var fs FrameSchema
		if err := json.Unmarshal(data, &fs); err != nil {
			return nil, fmt.Errorf("frames: parse %s: %w", ent.Name(), err)
		}
		if err := validate(fs); err != nil {
			return nil, fmt.Errorf("frames: invalid built-in %s: %w", ent.Name(), err)
		}
		r.byName[fs.Name] = fs
		r.builtin[fs.Name] = struct{}{}
	}
	// Sanity: exactly 11 built-ins with the expected names.
	if len(r.byName) != len(BuiltinNames) {
		return nil, fmt.Errorf("frames: expected %d built-ins, got %d", len(BuiltinNames), len(r.byName))
	}
	for _, n := range BuiltinNames {
		if _, ok := r.byName[n]; !ok {
			return nil, fmt.Errorf("frames: missing built-in %q", n)
		}
	}
	return r, nil
}

// LoadWithCustomDir builds on LoadBuiltin by additionally reading operator
// custom frame files from customDir. A custom file redefining a built-in
// name returns ErrBuiltinRedefined. A missing directory is not an error.
func LoadWithCustomDir(customDir string) (*Registry, error) {
	r, err := LoadBuiltin()
	if err != nil {
		return nil, err
	}
	if customDir == "" {
		return r, nil
	}
	entries, err := os.ReadDir(customDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return nil, fmt.Errorf("frames: read custom dir: %w", err)
	}
	// Stable order: sort filenames so errors are deterministic.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(customDir, n))
		if err != nil {
			return nil, fmt.Errorf("frames: read %s: %w", n, err)
		}
		var fs FrameSchema
		if err := json.Unmarshal(data, &fs); err != nil {
			return nil, fmt.Errorf("frames: parse %s: %w", n, err)
		}
		if err := validate(fs); err != nil {
			return nil, fmt.Errorf("frames: invalid custom frame %s: %w", n, err)
		}
		if _, isBuiltin := r.builtin[fs.Name]; isBuiltin {
			return nil, fmt.Errorf("%w: %s", ErrBuiltinRedefined, fs.Name)
		}
		r.byName[fs.Name] = fs
	}
	return r, nil
}

func validate(s FrameSchema) error {
	if s.Name == "" {
		return errors.New("name is required")
	}
	switch s.Store {
	case StoreEpisodic, StoreSemantic, StoreProcedural:
	default:
		return fmt.Errorf("invalid store %q", s.Store)
	}
	if len(s.Required) == 0 {
		return errors.New("required slot list must not be empty")
	}
	if s.Version < 1 {
		return errors.New("version must be >= 1")
	}
	return nil
}

// Get returns a frame schema by name.
func (r *Registry) Get(name string) (FrameSchema, bool) {
	s, ok := r.byName[name]
	return s, ok
}

// IsBuiltin reports whether name is one of the 11 built-in frame types.
func (r *Registry) IsBuiltin(name string) bool {
	_, ok := r.builtin[name]
	return ok
}

// Len returns the total number of registered schemas.
func (r *Registry) Len() int { return len(r.byName) }

// CheckObserveKind is the gate used by the `cortex observe` write pipeline.
// It returns nil if name is a known, agent-writable kind; ErrReflectionOnly
// if name is a reflection-only kind; and a distinct error for unknown
// kinds. Callers surface these to the standard error envelope as
// REFLECTION_ONLY_KIND and UNKNOWN_KIND respectively.
func (r *Registry) CheckObserveKind(name string) error {
	s, ok := r.byName[name]
	if !ok {
		return fmt.Errorf("UNKNOWN_KIND: %q", name)
	}
	if s.ReflectionOnly {
		return fmt.Errorf("%w: %s", ErrReflectionOnly, name)
	}
	return nil
}
