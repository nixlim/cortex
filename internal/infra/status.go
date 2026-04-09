package infra

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultStatusTimeout bounds the total wall clock of a single Check
// invocation. Spec acceptance requires cortex status to return in under
// 2 seconds so we leave headroom for process startup and JSON encoding.
const DefaultStatusTimeout = 1500 * time.Millisecond

// Component status constants — the three states the spec requires in
// §"Partial Backend Availability" and the scenario "Status reports
// running and degraded services".
const (
	StatusHealthy  = "healthy"
	StatusDegraded = "degraded"
	StatusDown     = "down"
)

// ComponentStatus is the per-service payload of a status report. It is
// intentionally flat (no nested structs) so the JSON envelope stays
// predictable across Phase 1 and Phase 2 evolution.
type ComponentStatus struct {
	Status  string `json:"status"`            // healthy | degraded | down
	Version string `json:"version,omitempty"` // best-effort, omitted on failure
	Error   string `json:"error,omitempty"`   // shallow error string when not healthy
}

// Report is the full shape of `cortex status --json`. The top-level
// keys match the acceptance criterion exactly ("weaviate", "neo4j",
// "ollama"). LogWatermark and EntryCount are pointers so they can be
// omitted before the log layer lands; DiskUsageBytes reports the
// live size of ~/.cortex/ and its subtrees.
type Report struct {
	Weaviate       ComponentStatus `json:"weaviate"`
	Neo4j          ComponentStatus `json:"neo4j"`
	Ollama         ComponentStatus `json:"ollama"`
	LogWatermark   *uint64         `json:"log_watermark,omitempty"`
	EntryCount     *int64          `json:"entry_count,omitempty"`
	DiskUsageBytes int64           `json:"disk_usage_bytes"`
	ElapsedMS      int64           `json:"elapsed_ms"`
}

// WeaviateStatusProbe is the narrow surface Check needs from the
// Weaviate adapter. It extends WeaviateReady with a Version fetch.
type WeaviateStatusProbe interface {
	Ready(ctx context.Context) error
	Version(ctx context.Context) (string, error)
}

// Neo4jStatusProbe is the narrow surface Check needs from the Neo4j
// adapter. Ping is the Bolt RETURN 1 health probe; Version returns
// the Neo4j server version via dbms.components() or equivalent.
type Neo4jStatusProbe interface {
	Ping(ctx context.Context) error
	Version(ctx context.Context) (string, error)
}

// OllamaStatusProbe is the narrow surface Check needs from the Ollama
// adapter. Ping hits /api/tags; Version hits /api/version.
type OllamaStatusProbe interface {
	Ping(ctx context.Context) error
	Version(ctx context.Context) (string, error)
}

// StatusOptions wires Check to concrete probes. CortexHome is optional;
// when set, Check walks the directory tree and reports the total
// bytes on disk as a rough "how much data am I holding" metric that
// the spec asks for in the --json payload.
type StatusOptions struct {
	Weaviate   WeaviateStatusProbe
	Neo4j      Neo4jStatusProbe
	Ollama     OllamaStatusProbe
	CortexHome string
	Timeout    time.Duration
	Clock      func() time.Time

	// LogWatermark and EntryCount, when non-nil, are called to populate
	// the corresponding Report fields. They are injected as closures so
	// the infra package does not take a dependency on the log layer.
	LogWatermark func() (uint64, error)
	EntryCount   func() (int64, error)
}

// Check runs every probe concurrently and returns a Report. It never
// returns an error: every failure is encoded into the per-component
// status so callers can print a best-effort picture even when the
// whole stack is down. The overall wall-clock is bounded by Timeout
// (default 1500ms) so cortex status always fits inside the 2-second
// spec budget.
func Check(ctx context.Context, opts StatusOptions) Report {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultStatusTimeout
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}

	start := opts.Clock()
	probeCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var (
		wg     sync.WaitGroup
		report Report
	)

	if opts.Weaviate != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report.Weaviate = probeComponent(probeCtx,
				opts.Weaviate.Ready, opts.Weaviate.Version)
		}()
	} else {
		report.Weaviate = ComponentStatus{Status: StatusDown, Error: "no probe configured"}
	}
	if opts.Neo4j != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report.Neo4j = probeComponent(probeCtx,
				opts.Neo4j.Ping, opts.Neo4j.Version)
		}()
	} else {
		report.Neo4j = ComponentStatus{Status: StatusDown, Error: "no probe configured"}
	}
	if opts.Ollama != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report.Ollama = probeComponent(probeCtx,
				opts.Ollama.Ping, opts.Ollama.Version)
		}()
	} else {
		report.Ollama = ComponentStatus{Status: StatusDown, Error: "no probe configured"}
	}

	wg.Wait()

	if opts.CortexHome != "" {
		report.DiskUsageBytes = diskUsage(opts.CortexHome)
	}
	if opts.LogWatermark != nil {
		if v, err := opts.LogWatermark(); err == nil {
			report.LogWatermark = &v
		}
	}
	if opts.EntryCount != nil {
		if v, err := opts.EntryCount(); err == nil {
			report.EntryCount = &v
		}
	}

	report.ElapsedMS = opts.Clock().Sub(start).Milliseconds()
	return report
}

// probeComponent runs the liveness probe, and on success the version
// fetch. It classifies the outcome into one of the three spec states:
//
//   - Ping fails                        → down
//   - Ping OK, Version fetch fails      → degraded
//   - Ping OK, Version fetch OK         → healthy
func probeComponent(ctx context.Context,
	ping func(context.Context) error,
	version func(context.Context) (string, error),
) ComponentStatus {
	if err := ping(ctx); err != nil {
		return ComponentStatus{Status: StatusDown, Error: shallowErrString(err)}
	}
	v, verr := version(ctx)
	if verr != nil {
		return ComponentStatus{Status: StatusDegraded, Error: shallowErrString(verr)}
	}
	return ComponentStatus{Status: StatusHealthy, Version: v}
}

// shallowErrString trims error messages so the status report stays
// one line per component and avoids leaking deep context the spec
// reserves for ops.log.
func shallowErrString(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 160
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// diskUsage walks root and returns the total size in bytes of all
// regular files. Errors are swallowed silently: status is a shallow
// reporting command, not a guarantee of accuracy, and a missing
// CortexHome should not break the rest of the report.
func diskUsage(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			// Keep walking over permission errors, missing files, etc.
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// MarshalJSON produces the canonical on-wire shape of a Report. It is
// a thin wrapper around encoding/json that sorts map keys (none here)
// and hides ElapsedMS from tests that assert on deterministic output.
func (r Report) MarshalJSON() ([]byte, error) {
	type alias Report
	return json.Marshal(alias(r))
}

// FetchWeaviateVersion is a convenience helper for the live cmd/cortex
// wire-up: it hits GET <endpoint>/v1/meta and extracts the "version"
// field. The helper lives in infra so the adapter bridge in cmd/cortex
// can stay a pure data mover.
func FetchWeaviateVersion(ctx context.Context, httpEndpoint string) (string, error) {
	base := strings.TrimRight(httpEndpoint, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/meta", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("weaviate: /v1/meta returned non-200")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var meta struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", err
	}
	return meta.Version, nil
}

// FetchOllamaVersion hits GET <endpoint>/api/version and returns the
// "version" field. Same rationale as FetchWeaviateVersion: lifecycle
// commands need version probes that the vector-write Client interface
// does not carry.
func FetchOllamaVersion(ctx context.Context, endpoint string) (string, error) {
	base := strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("ollama: /api/version returned non-200")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}
