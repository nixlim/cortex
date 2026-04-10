package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOllama is an httptest-backed double for the Ollama HTTP API.
// Tests flip fields on it (tagsStatus, showDigest, embedDigest,
// embedLength) between constructions rather than using a
// gomock-style expectation setter — the API surface is small enough
// that plain struct fields are clearer than a mocking framework.
type fakeOllama struct {
	server *httptest.Server

	tagsStatus  int32
	showCalls   int32
	embedCalls  int32
	showDigest  string
	showName    string
	embedDigest string // if non-empty, included on /api/embeddings responses
	embedLength int    // zero → return 768-dim vector

	generateBody string
}

func newFakeOllama() *fakeOllama {
	f := &fakeOllama{
		tagsStatus:   http.StatusOK,
		showDigest:   "sha256:aabbccdd",
		showName:     "nomic-embed-text",
		embedLength:  768,
		generateBody: "ok",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&f.tagsStatus)))
		_, _ = io.WriteString(w, `{"models":[]}`)
	})

	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.showCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(showResponse{
			Name:   f.showName,
			Digest: f.showDigest,
		})
	})

	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.embedCalls, 1)
		length := f.embedLength
		if length == 0 {
			length = 768
		}
		vec := make([]float32, length)
		for i := range vec {
			vec[i] = float32(i) / float32(length)
		}
		resp := embedResponse{Embedding: vec, Digest: f.embedDigest}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generateResponse{
			Response: f.generateBody,
			Done:     true,
		})
	})

	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeOllama) Close() { f.server.Close() }

func newClientFor(f *fakeOllama) *HTTPClient {
	return NewHTTPClient(Config{
		Endpoint:              f.server.URL,
		EmbeddingModel:        "nomic-embed-text",
		GenerationModel:       "qwen3:4b-instruct-2507",
		EmbeddingTimeout:      2 * time.Second,
		LinkDerivationTimeout: 2 * time.Second,
	})
}

// TestEmbed_Returns768Dim covers the acceptance criterion "Embed
// returns a vector of length 768 for nomic-embed-text on a
// non-empty input string".
func TestEmbed_Returns768Dim(t *testing.T) {
	f := newFakeOllama()
	defer f.Close()

	c := newClientFor(f)
	vec, err := c.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 768 {
		t.Fatalf("Embed returned %d-dim vector, want 768", len(vec))
	}
}

// TestShowCalledExactlyOnce covers the acceptance criterion "Show is
// called exactly once per invocation (verified via call counter
// seam)". We issue three Embed calls back-to-back on the same
// client; the underlying /api/show handler must see exactly one
// request, not three.
func TestShowCalledExactlyOnce(t *testing.T) {
	f := newFakeOllama()
	defer f.Close()

	c := newClientFor(f)
	for i := 0; i < 3; i++ {
		if _, err := c.Embed(context.Background(), "x"); err != nil {
			t.Fatalf("Embed %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&f.showCalls); got != 1 {
		t.Fatalf("fake saw %d /api/show calls, want 1", got)
	}
	if got := c.ShowCallCount(); got != 1 {
		t.Fatalf("client ShowCallCount = %d, want 1", got)
	}
}

// TestEmbed_DigestRace covers the acceptance criterion "When the
// digest reported on an Embed response differs from the cached
// digest, the call returns ErrModelDigestRace". The fake is
// configured to report a different digest on /api/embeddings than
// on /api/show.
func TestEmbed_DigestRace(t *testing.T) {
	f := newFakeOllama()
	defer f.Close()
	f.showDigest = "sha256:AAAA"
	f.embedDigest = "sha256:BBBB" // differs → race

	c := newClientFor(f)
	_, err := c.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("expected ErrModelDigestRace, got nil")
	}
	if !errors.Is(err, ErrModelDigestRace) {
		t.Fatalf("err = %v, want ErrModelDigestRace", err)
	}
}

// TestEmbed_NoDigestOnResponseIsOK documents the tolerance path:
// some Ollama versions don't emit the digest on /api/embeddings
// responses at all. When the response digest is empty we MUST NOT
// fail the call, because the cached Show digest is already the
// single source of truth for this invocation.
func TestEmbed_NoDigestOnResponseIsOK(t *testing.T) {
	f := newFakeOllama()
	defer f.Close()
	f.showDigest = "sha256:aaaa"
	f.embedDigest = "" // omit

	c := newClientFor(f)
	if _, err := c.Embed(context.Background(), "x"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
}

// TestPing_ReachableAndUnreachable covers the acceptance criterion
// "Ping returns nil with a reachable Ollama and a non-nil error
// when the service is down".
func TestPing_ReachableAndUnreachable(t *testing.T) {
	f := newFakeOllama()
	c := newClientFor(f)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping against reachable fake: %v", err)
	}

	f.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("Ping against stopped fake returned nil, want error")
	}
}

func TestGenerate_ReturnsResponseField(t *testing.T) {
	f := newFakeOllama()
	defer f.Close()
	f.generateBody = "derived link: RELATES_TO"

	c := newClientFor(f)
	got, err := c.Generate(context.Background(), "prompt text")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "derived link: RELATES_TO" {
		t.Errorf("Generate returned %q", got)
	}
}

func TestShow_ErrorsCachedAcrossCalls(t *testing.T) {
	// If the first Show call fails, the error must be cached so
	// repeat callers see the same failure without spamming the
	// network. This preserves the "exactly once" guarantee even
	// under error conditions.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/show") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"nope"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewHTTPClient(Config{
		Endpoint:         srv.URL,
		EmbeddingModel:   "nomic-embed-text",
		EmbeddingTimeout: time.Second,
	})
	_, err1 := c.Show(context.Background())
	_, err2 := c.Show(context.Background())
	if err1 == nil || err2 == nil {
		t.Fatal("expected both calls to error")
	}
	if err1.Error() != err2.Error() {
		t.Errorf("cached error mismatch: %v vs %v", err1, err2)
	}
	if got := c.ShowCallCount(); got != 1 {
		t.Errorf("ShowCallCount = %d, want 1 (even after repeat failures)", got)
	}
}

func TestDigestsEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"sha256:abc", "sha256:abc", true},
		{"sha256:abc", "abc", true}, // tolerate missing algo prefix
		{"abc", "abc", true},
		{"sha256:abc", "sha256:def", false},
		{"", "", false}, // empty digests are NOT equal (can't verify)
		{"sha256:", "sha256:", false},
	}
	for _, c := range cases {
		if got := DigestsEqual(c.a, c.b); got != c.want {
			t.Errorf("DigestsEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
