package infra

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nixlim/cortex/internal/errs"
)

// DefaultStartupBudget is the wall-clock budget for cortex up, from
// docs/spec/cortex-spec.md §"cortex up Readiness Contract" step 5.
const DefaultStartupBudget = 90 * time.Second

// Stable error codes emitted by Run. Each code maps 1:1 to a readiness
// contract failure mode and is asserted by acceptance tests.
const (
	CodeDockerUnreachable     = "DOCKER_UNREACHABLE"
	CodeComposeFailed         = "COMPOSE_FAILED"
	CodeWeaviateNotReady      = "WEAVIATE_NOT_READY"
	CodeNeo4jNotReady         = "NEO4J_NOT_READY"
	CodeGDSNotAvailable       = "GDS_NOT_AVAILABLE"
	CodeOllamaNotReachable    = "OLLAMA_NOT_REACHABLE"
	CodeOllamaModelMissing    = "OLLAMA_MODEL_MISSING"
	CodeStartupBudgetExceeded = "STARTUP_BUDGET_EXCEEDED"
	CodeCredentialWriteFailed = "NEO4J_CREDENTIAL_WRITE_FAILED"
	CodeUpMisconfigured       = "UP_MISCONFIGURED"
)

// WeaviateReady is the minimum surface Run needs from the Weaviate
// adapter. internal/weaviate.HTTPClient satisfies it via Ready.
type WeaviateReady interface {
	Ready(ctx context.Context) error
}

// Neo4jReady is the minimum surface Run needs from the Neo4j adapter.
// Production callers wrap internal/neo4j.BoltClient in a small adapter
// because the live Client surfaces GDS as a ProcedureAvailability
// struct rather than a boolean.
type Neo4jReady interface {
	Ping(ctx context.Context) error
	// GDSAvailable reports whether at least one GDS procedure is
	// callable on the live Neo4j instance. Returning (false, nil) is
	// a valid "plugin not installed" signal and maps to
	// GDS_NOT_AVAILABLE without a cause chain.
	GDSAvailable(ctx context.Context) (bool, error)
}

// OllamaReady is the minimum surface Run needs from the Ollama adapter.
// The live HTTPClient satisfies Ping; model enumeration lives here
// (rather than on the ollama.Client interface) because it is only used
// by lifecycle commands.
type OllamaReady interface {
	Ping(ctx context.Context) error
	ListModels(ctx context.Context) ([]string, error)
}

// UpOptions wires Run to a concrete environment. Every dependency that
// touches the host (docker, backends, filesystem) is injected so that
// unit tests can exercise the full orchestration logic with in-memory
// fakes.
type UpOptions struct {
	// ComposeFile is the docker-compose file Run will `compose up -d`.
	ComposeFile string

	// ConfigPath is the canonical ~/.cortex/config.yaml location. Run
	// uses it for first-run Neo4j password generation.
	ConfigPath string

	// StartupBudget caps the total wall clock from Run entry to final
	// readiness. Defaults to DefaultStartupBudget (90s) when zero.
	StartupBudget time.Duration

	// ProbeInterval is how long Run waits between readiness probes.
	// Defaults to 500ms when zero.
	ProbeInterval time.Duration

	// EmbeddingModel is the Ollama model that MUST be present. If
	// missing, Run returns OLLAMA_MODEL_MISSING with an `ollama pull`
	// remediation string.
	EmbeddingModel string

	// GenerationModel is the Ollama model used for reflection / link
	// derivation. If empty, the generation-model presence check is
	// skipped; if non-empty and missing, Run returns
	// OLLAMA_MODEL_MISSING (same code, listed in the same failure).
	GenerationModel string

	// Docker, Weaviate, Neo4j, Ollama are the injected adapters.
	Docker   DockerRunner
	Weaviate WeaviateReady
	Neo4j    Neo4jReady
	Ollama   OllamaReady

	// Clock is injected for deterministic deadline tests; defaults to
	// time.Now.
	Clock func() time.Time

	// OnPasswordGenerated is invoked once, inside Run, when a brand-new
	// Neo4j password has been persisted to ConfigPath. Callers typically
	// use it to log a one-line notice that a new credential exists
	// without leaking the password itself.
	OnPasswordGenerated func(configPath string)
}

// Run executes the cortex up readiness contract end-to-end. A nil
// return indicates every managed service passed its probe within the
// wall-clock budget. A non-nil return is always an *errs.Error whose
// Kind and Code are set for mapping to the process exit code and
// envelope emission at the CLI boundary.
//
// Run is deliberately all-or-nothing: the spec requires that partial
// success (e.g. Weaviate ready, Neo4j not) fails the command with a
// non-zero exit. Callers should NOT interpret a non-nil return as a
// degraded mode; that is what cortex status and cortex doctor report.
func Run(ctx context.Context, opts UpOptions) error {
	if opts.StartupBudget <= 0 {
		opts.StartupBudget = DefaultStartupBudget
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = 500 * time.Millisecond
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.EmbeddingModel == "" {
		return errs.Validation(CodeUpMisconfigured,
			"cortex up requires an embedding model to be configured", nil)
	}
	if opts.Docker == nil || opts.Weaviate == nil || opts.Neo4j == nil || opts.Ollama == nil {
		return errs.Validation(CodeUpMisconfigured,
			"cortex up requires docker, weaviate, neo4j, and ollama adapters", nil)
	}

	deadline := opts.Clock().Add(opts.StartupBudget)
	budgetCtx, cancelBudget := context.WithDeadline(ctx, deadline)
	defer cancelBudget()

	// Step 1: Docker daemon reachability. If this fails nothing else
	// can possibly succeed — return immediately rather than burning the
	// whole startup budget on doomed probes.
	if err := opts.Docker.Ping(budgetCtx); err != nil {
		return errs.Operational(CodeDockerUnreachable,
			"docker daemon is not reachable", err)
	}

	// Step 2: Ensure a Neo4j bootstrap password exists. This runs
	// before compose up so that the value can be injected into the
	// container environment via the compose file's NEO4J_PASSWORD
	// placeholder. Failure here is reported as a credential-write
	// error, not a readiness failure.
	neo4jPassword, generated, err := EnsureNeo4jPassword(opts.ConfigPath)
	if err != nil {
		return errs.Operational(CodeCredentialWriteFailed,
			"failed to persist neo4j bootstrap credential", err)
	}
	if generated && opts.OnPasswordGenerated != nil {
		opts.OnPasswordGenerated(opts.ConfigPath)
	}

	// Step 3: Compose up. A failure here is reported as COMPOSE_FAILED
	// before any readiness probing starts, because there is no
	// container to probe. NEO4J_PASSWORD is threaded into the compose
	// subprocess env so the compose file's ${NEO4J_PASSWORD:-...}
	// placeholder expands to the freshly generated credential instead
	// of the literal bootstrap fallback (which would otherwise burn
	// into the neo4j_data volume on first boot).
	composeEnv := map[string]string{"NEO4J_PASSWORD": neo4jPassword}
	if err := opts.Docker.ComposeUp(budgetCtx, opts.ComposeFile, composeEnv); err != nil {
		return errs.Operational(CodeComposeFailed,
			"docker compose up failed", err)
	}

	// Step 4: Probe Weaviate, Neo4j, and Ollama concurrently. Each
	// probe polls its service until it succeeds OR the shared budget
	// ctx expires. The first probe to report a hard failure cancels
	// the shared probe ctx so sibling probes stop spinning.
	probeCtx, cancelProbes := context.WithCancel(budgetCtx)
	defer cancelProbes()

	var (
		once     sync.Once
		firstErr *errs.Error
	)
	fail := func(e *errs.Error) {
		once.Do(func() {
			firstErr = e
			cancelProbes()
		})
	}

	var wg sync.WaitGroup

	// ---------------------------- Weaviate ---------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := probeUntilReady(probeCtx, opts.ProbeInterval, opts.Weaviate.Ready); err != nil {
			if siblingCanceled(budgetCtx, probeCtx) {
				return
			}
			fail(operationalOrBudget(budgetCtx,
				CodeWeaviateNotReady,
				"weaviate never became ready within the startup budget",
				err))
		}
	}()

	// ---------------------------- Neo4j ------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := probeUntilReady(probeCtx, opts.ProbeInterval, opts.Neo4j.Ping); err != nil {
			if siblingCanceled(budgetCtx, probeCtx) {
				return
			}
			fail(operationalOrBudget(budgetCtx,
				CodeNeo4jNotReady,
				"neo4j never became ready within the startup budget",
				err))
			return
		}
		// Neo4j answers Bolt — now verify the GDS plugin.
		avail, gdsErr := opts.Neo4j.GDSAvailable(probeCtx)
		if gdsErr != nil {
			if siblingCanceled(budgetCtx, probeCtx) {
				return
			}
			fail(errs.Operational(CodeGDSNotAvailable,
				"neo4j gds.version() probe failed", gdsErr))
			return
		}
		if !avail {
			fail(errs.Operational(CodeGDSNotAvailable,
				"neo4j GDS plugin is not installed on the running instance", nil))
		}
	}()

	// ---------------------------- Ollama -----------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := probeUntilReady(probeCtx, opts.ProbeInterval, opts.Ollama.Ping); err != nil {
			if siblingCanceled(budgetCtx, probeCtx) {
				return
			}
			fail(operationalOrBudget(budgetCtx,
				CodeOllamaNotReachable,
				"ollama is not reachable on the configured endpoint",
				err))
			return
		}
		models, listErr := opts.Ollama.ListModels(probeCtx)
		if listErr != nil {
			if siblingCanceled(budgetCtx, probeCtx) {
				return
			}
			fail(errs.Operational(CodeOllamaNotReachable,
				"failed to list ollama models", listErr))
			return
		}
		var missing []string
		if !containsModel(models, opts.EmbeddingModel) {
			missing = append(missing, opts.EmbeddingModel)
		}
		if opts.GenerationModel != "" && !containsModel(models, opts.GenerationModel) {
			missing = append(missing, opts.GenerationModel)
		}
		if len(missing) > 0 {
			msg := fmt.Sprintf(
				"required ollama model(s) missing: %s; run: ollama pull %s",
				strings.Join(missing, ", "),
				missing[0],
			)
			fail(errs.Operational(CodeOllamaModelMissing, msg, nil))
		}
	}()

	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return nil
}

// probeUntilReady repeatedly calls fn until it returns nil, the
// supplied context expires, or the context is canceled. It returns the
// last error observed from fn on context expiry so callers can classify
// the failure.
func probeUntilReady(ctx context.Context, interval time.Duration, fn func(context.Context) error) error {
	var lastErr error
	for {
		if err := fn(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr == nil {
				return ctx.Err()
			}
			return lastErr
		case <-time.After(interval):
		}
	}
}

// siblingCanceled reports whether the probe ctx has been canceled by a
// sibling probe failure (as opposed to the overall budget ctx expiring
// or the caller canceling). When a sibling has already set firstErr,
// the remaining goroutines should exit quietly without overwriting it.
func siblingCanceled(budgetCtx, probeCtx context.Context) bool {
	return probeCtx.Err() != nil && budgetCtx.Err() == nil
}

// operationalOrBudget classifies a probe failure as either a per-service
// not-ready error or a startup-budget error, depending on whether the
// budget ctx itself has expired. A deadline-exceeded context means the
// 90-second budget was consumed; anything else is the service-specific
// probe failing on its own merits.
func operationalOrBudget(budgetCtx context.Context, code, msg string, cause error) *errs.Error {
	if errors.Is(budgetCtx.Err(), context.DeadlineExceeded) {
		return errs.Operational(CodeStartupBudgetExceeded,
			"startup budget exceeded while waiting for services to become ready",
			cause)
	}
	return errs.Operational(code, msg, cause)
}

// containsModel reports whether Ollama's /api/tags listing covers a
// configured model name. Ollama tags include an optional ":tag" suffix
// (e.g. "nomic-embed-text:latest"); cortex config usually names the
// model without a tag, so we treat both forms as equivalent.
func containsModel(installed []string, want string) bool {
	for _, got := range installed {
		if got == want {
			return true
		}
		if strings.HasPrefix(got, want+":") {
			return true
		}
		if want == got+":latest" {
			return true
		}
	}
	return false
}
