// Package secrets implements the built-in secret-detector ruleset for
// Cortex observe/ingest/LLM-output scrubbing.
//
// The built-in rules live in builtin.yaml, which is embedded at build time;
// operators may add project-specific rules via ~/.cortex/secrets.yaml but
// cannot redefine built-in rule names (Phase 1).
//
// The detector exposes Scan(body) which returns a slice of Match values.
// Each Match carries only the rule name and the byte offsets of the hit —
// the matched substring itself is never stored or returned, to prevent
// secret payloads from leaking into error envelopes, ops.log, or any
// derived index.
package secrets

import (
	_ "embed"
	"errors"
	"fmt"
	"math"
	"regexp"

	"gopkg.in/yaml.v3"
)

//go:embed builtin.yaml
var builtinYAML []byte

// Rule is one regex-plus-metadata entry.
type Rule struct {
	Name     string  `yaml:"name"`
	Regex    string  `yaml:"regex"`
	Entropy  float64 `yaml:"entropy"`
	Severity string  `yaml:"severity"`

	compiled *regexp.Regexp
}

// Match is a redacted detector hit. The raw substring is deliberately
// omitted.
type Match struct {
	Rule     string
	Severity string
	Start    int
	End      int
}

// Detector scans content against a compiled ruleset.
type Detector struct {
	rules            []Rule
	builtinNames     map[string]struct{}
	entropyThreshold float64
}

// LoadBuiltin returns a Detector populated from the embedded built-in
// ruleset only. entropyThreshold is the default generic-high-entropy
// floor; pass 0 to use the spec default (4.5).
func LoadBuiltin(entropyThreshold float64) (*Detector, error) {
	if entropyThreshold <= 0 {
		entropyThreshold = 4.5
	}
	rules, err := parseRules(builtinYAML)
	if err != nil {
		return nil, fmt.Errorf("secrets: parse builtin: %w", err)
	}
	names := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		names[r.Name] = struct{}{}
	}
	return &Detector{
		rules:            rules,
		builtinNames:     names,
		entropyThreshold: entropyThreshold,
	}, nil
}

// MergeCustom additively merges custom rules from customYAML into the
// detector. A custom rule whose name collides with a built-in returns
// ErrCustomRedefinesBuiltin.
func (d *Detector) MergeCustom(customYAML []byte) error {
	if len(customYAML) == 0 {
		return nil
	}
	custom, err := parseRules(customYAML)
	if err != nil {
		return fmt.Errorf("secrets: parse custom: %w", err)
	}
	for _, r := range custom {
		if _, isBuiltin := d.builtinNames[r.Name]; isBuiltin {
			return fmt.Errorf("%w: %s", ErrCustomRedefinesBuiltin, r.Name)
		}
		d.rules = append(d.rules, r)
	}
	return nil
}

// ErrCustomRedefinesBuiltin is returned when a custom ruleset tries to
// override a built-in rule name.
var ErrCustomRedefinesBuiltin = errors.New("secrets: custom ruleset cannot redefine built-in rule")

// Scan searches body for rule hits and returns redacted Match values.
// The matched substring is never part of the result. For generic
// high-entropy rules (non-zero Entropy), the match is kept only when
// the matched substring's Shannon entropy is at or above the rule's
// floor.
func (d *Detector) Scan(body string) []Match {
	var out []Match
	for _, r := range d.rules {
		re := r.compiled
		if re == nil {
			continue
		}
		locs := re.FindAllStringIndex(body, -1)
		for _, loc := range locs {
			if r.Entropy > 0 {
				e := shannon(body[loc[0]:loc[1]])
				if e < r.Entropy {
					continue
				}
			}
			out = append(out, Match{
				Rule:     r.Name,
				Severity: r.Severity,
				Start:    loc[0],
				End:      loc[1],
			})
		}
	}
	return out
}

// RuleNames returns a sorted-stable list of all loaded rule names. Useful
// for the `cortex doctor` self-check.
func (d *Detector) RuleNames() []string {
	out := make([]string, 0, len(d.rules))
	for _, r := range d.rules {
		out = append(out, r.Name)
	}
	return out
}

type rulesFile struct {
	Rules []Rule `yaml:"rules"`
}

func parseRules(data []byte) ([]Rule, error) {
	var rf rulesFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, err
	}
	for i := range rf.Rules {
		if rf.Rules[i].Name == "" || rf.Rules[i].Regex == "" {
			return nil, fmt.Errorf("rule %d: name and regex are required", i)
		}
		re, err := regexp.Compile(rf.Rules[i].Regex)
		if err != nil {
			return nil, fmt.Errorf("rule %q: compile regex: %w", rf.Rules[i].Name, err)
		}
		rf.Rules[i].compiled = re
	}
	return rf.Rules, nil
}

// shannon computes the Shannon entropy of s in bits per byte. Runs in
// O(n) with a 256-entry histogram — no extra allocations per call.
func shannon(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var hist [256]int
	for i := 0; i < len(s); i++ {
		hist[s[i]]++
	}
	var e float64
	n := float64(len(s))
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		e -= p * math.Log2(p)
	}
	return e
}
