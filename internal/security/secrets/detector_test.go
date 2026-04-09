package secrets

import (
	"errors"
	"strings"
	"testing"
)

func mustDetector(t *testing.T) *Detector {
	t.Helper()
	d, err := LoadBuiltin(0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAWSAccessKeyMatch(t *testing.T) {
	d := mustDetector(t)
	hits := d.Scan("here is a token AKIAIOSFODNN7EXAMPLE inline")
	if len(hits) == 0 {
		t.Fatal("expected at least one match")
	}
	found := false
	for _, h := range hits {
		if h.Rule == "aws_access_key" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected aws_access_key rule, got %+v", hits)
	}
}

func TestPrivateKeyPEMMatch(t *testing.T) {
	d := mustDetector(t)
	body := "log:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n"
	hits := d.Scan(body)
	found := false
	for _, h := range hits {
		if h.Rule == "private_key_pem" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected private_key_pem match, got %+v", hits)
	}
}

func TestMatchResultHasNoRawString(t *testing.T) {
	d := mustDetector(t)
	body := "AKIAIOSFODNN7EXAMPLE"
	hits := d.Scan(body)
	if len(hits) == 0 {
		t.Fatal("no matches")
	}
	// The Match struct has no field that can hold the raw substring.
	// Compile-time guarantee — but also sanity-check that rule names
	// never appear to contain the matched value.
	for _, h := range hits {
		if strings.Contains(h.Rule, "AKIA") {
			t.Errorf("rule name leaked matched substring: %q", h.Rule)
		}
		if h.Start < 0 || h.End > len(body) || h.End <= h.Start {
			t.Errorf("bad offsets: %+v", h)
		}
	}
}

func TestCustomRulesAdditive(t *testing.T) {
	d := mustDetector(t)
	custom := []byte(`rules:
  - name: company_internal_token
    regex: 'acme-[A-Z]{10}'
    severity: high
`)
	if err := d.MergeCustom(custom); err != nil {
		t.Fatal(err)
	}
	names := d.RuleNames()
	hasCustom := false
	for _, n := range names {
		if n == "company_internal_token" {
			hasCustom = true
		}
	}
	if !hasCustom {
		t.Error("custom rule not registered")
	}
	hits := d.Scan("token acme-ABCDEFGHIJ present")
	if len(hits) == 0 {
		t.Error("custom rule did not match")
	}
}

func TestCustomRulesCannotRedefineBuiltin(t *testing.T) {
	d := mustDetector(t)
	custom := []byte(`rules:
  - name: aws_access_key
    regex: 'AKIA.*'
    severity: info
`)
	err := d.MergeCustom(custom)
	if !errors.Is(err, ErrCustomRedefinesBuiltin) {
		t.Fatalf("expected ErrCustomRedefinesBuiltin, got %v", err)
	}
}

func TestEntropyFiltersGenericRule(t *testing.T) {
	d := mustDetector(t)
	// Low-entropy match for the generic rule should be filtered.
	lowEntropy := "password: aaaaaaaaaaaaaaaaaaaaaaaa"
	hits := d.Scan(lowEntropy)
	for _, h := range hits {
		if h.Rule == "generic_high_entropy_secret" {
			t.Errorf("low-entropy match should have been filtered: %+v", h)
		}
	}
	// High-entropy match should pass.
	highEntropy := "password: Kd93jDkal92MzPoQ8rTyY7xvB3Zq"
	hits2 := d.Scan(highEntropy)
	ok := false
	for _, h := range hits2 {
		if h.Rule == "generic_high_entropy_secret" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("high-entropy match should have been detected: %+v", hits2)
	}
}

func TestShannonEntropyOfHomogeneousString(t *testing.T) {
	if e := shannon("aaaaaaa"); e != 0 {
		t.Errorf("shannon(constant) = %v, want 0", e)
	}
	if e := shannon(""); e != 0 {
		t.Errorf("shannon(empty) = %v, want 0", e)
	}
}
