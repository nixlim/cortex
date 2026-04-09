package psi

import (
	"errors"
	"testing"
)

func TestValidateLibGoRedis(t *testing.T) {
	p, err := Validate("lib/go/redis")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Namespace != "lib" {
		t.Errorf("namespace: got %q want %q", p.Namespace, "lib")
	}
	if p.LocalID != "go/redis" {
		t.Errorf("local_id: got %q want %q", p.LocalID, "go/redis")
	}
	if p.CanonicalForm != "lib/go/redis" {
		t.Errorf("canonical: %q", p.CanonicalForm)
	}
}

func TestValidateUnknownNamespace(t *testing.T) {
	_, err := Validate("foo/bar")
	if !errors.Is(err, ErrUnknownNamespace) {
		t.Fatalf("expected ErrUnknownNamespace, got %v", err)
	}
}

func TestValidateAllRequiredNamespaces(t *testing.T) {
	for _, ns := range Namespaces {
		p, err := Validate(ns + "/x")
		if err != nil {
			t.Errorf("namespace %q rejected: %v", ns, err)
		}
		if p.Namespace != ns {
			t.Errorf("round-trip namespace wrong: %q", p.Namespace)
		}
	}
}

func TestValidateEmptyLocalID(t *testing.T) {
	_, err := Validate("lib/")
	if !errors.Is(err, ErrEmptyLocalID) {
		t.Fatalf("expected ErrEmptyLocalID, got %v", err)
	}
}

func TestImmutableCanonical(t *testing.T) {
	r := NewRegistry()
	p, _ := Validate("lib/go/redis")
	if err := r.Mint(p); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Attempting to mutate the existing canonical slot must fail.
	err := r.ForceMutate("lib/go/redis", "lib/go/goredis")
	if !errors.Is(err, ErrImmutableCanonical) {
		t.Fatalf("expected ErrImmutableCanonical, got %v", err)
	}
}

func TestAliasResolvesToCanonical(t *testing.T) {
	r := NewRegistry()
	canonical, _ := Validate("lib/go/redis")
	if err := r.Mint(canonical); err != nil {
		t.Fatal(err)
	}
	if err := r.AddAlias("lib/go/redigo", "lib/go/redis"); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Canonical("lib/go/redigo")
	if !ok {
		t.Fatal("alias not found")
	}
	if got != "lib/go/redis" {
		t.Errorf("alias resolved to %q, want lib/go/redis", got)
	}

	// Canonical passes through.
	got2, ok := r.Canonical("lib/go/redis")
	if !ok || got2 != "lib/go/redis" {
		t.Errorf("canonical passthrough failed: %q ok=%v", got2, ok)
	}

	// Unknown returns unchanged.
	unk, ok := r.Canonical("lib/go/other")
	if ok || unk != "lib/go/other" {
		t.Errorf("unknown resolution wrong: %q ok=%v", unk, ok)
	}
}
