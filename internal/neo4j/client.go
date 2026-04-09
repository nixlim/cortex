// Package neo4j is the Cortex adapter for the managed Neo4j + GDS service.
//
// The package wraps the official neo4j-go-driver v5 behind a small Go
// interface (Client) so higher layers can stay ignorant of driver
// types, auth token construction, and session lifetime management. A
// second, narrower interface (cypherRunner) sits between the adapter
// and the driver session so unit tests can substitute a fake without
// requiring a live Neo4j instance.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Neo4j with Graph Data Science"
//   docs/spec/cortex-spec.md §"cortex up Readiness Contract"
//   docs/spec/cortex-spec.md §"Custom Neo4j + GDS Image" (GDS probe)
package neo4j

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	neodrv "github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// DefaultTimeout is the fallback per-query budget used when the caller
// passes a zero duration to NewBoltClient.
const DefaultTimeout = 10 * time.Second

// Procedure name constants — these are the GDS procedures Cortex
// cares about in Phase 1. Exported so that cortex doctor / status can
// list them without duplicating the strings.
const (
	ProcPageRankStream = "gds.pageRank.stream"
	ProcLeidenStream   = "gds.leiden.stream"
	ProcLouvainStream  = "gds.louvain.stream"
)

// Client is the interface exposed by the adapter. Implementations MUST
// be safe for concurrent use — the driver's DriverWithContext is
// goroutine-safe and we rely on that.
type Client interface {
	// Ping opens a session, runs `RETURN 1`, and returns nil on a
	// successful row. An auth failure MUST surface as a non-nil error
	// (the driver distinguishes AuthenticationError internally).
	Ping(ctx context.Context) error

	// ProbeProcedures checks the availability of the GDS procedures
	// Cortex calls from the recall and community-detection pipelines.
	// Returns a ProcedureAvailability report so the caller can make
	// the Leiden-preferred / Louvain-fallback decision described in
	// FR-028.
	ProbeProcedures(ctx context.Context) (ProcedureAvailability, error)

	// WriteEntries is the primary write surface used by the write
	// pipeline. Phase 1 uses it to MERGE entry nodes and their
	// derived links; we keep it generic (parameterised Cypher) rather
	// than modelling one method per node type so the write pipeline
	// can evolve without churning this interface.
	WriteEntries(ctx context.Context, cypher string, params map[string]any) error

	// QueryGraph runs a read-only Cypher statement and returns each
	// row as a map. The result set is materialised so the caller
	// doesn't have to reason about session lifetime.
	QueryGraph(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error)

	// RunGDS executes a GDS procedure. It is a thin typed wrapper
	// around QueryGraph that lets call sites make their intent
	// explicit and lets telemetry attribute the query to the GDS
	// subsystem.
	RunGDS(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error)

	// Close releases the underlying driver's connection pool. Callers
	// should defer it once per process.
	Close(ctx context.Context) error
}

// ProcedureAvailability is the result of ProbeProcedures.
//
// LeidenUnavailable is a convenience signal: it is true when the
// Leiden procedure is missing but the Louvain fallback is available,
// exactly the condition FR-028 uses to switch community detection
// algorithms at runtime.
type ProcedureAvailability struct {
	PageRankStream    bool
	LeidenStream      bool
	LouvainStream     bool
	LeidenUnavailable bool
}

// ErrLeidenUnavailable is surfaced by higher layers (community
// detection) when LeidenUnavailable is true and no fallback is
// acceptable. The adapter itself does not return it from
// ProbeProcedures; it returns the full ProcedureAvailability struct
// and lets the caller decide.
var ErrLeidenUnavailable = errors.New("neo4j: gds.leiden.stream unavailable, falling back to Louvain")

// Config is the subset of config fields this adapter needs. Keeping
// this as a small local struct (rather than importing internal/config)
// lets foundation-dev evolve the config surface without breaking this
// package and keeps the adapter unit-testable without a full Config
// tree.
type Config struct {
	BoltEndpoint string        // e.g. "localhost:7687" or "bolt://localhost:7687"
	Username     string        // e.g. "neo4j"
	Password     string        // stored in ~/.cortex/config.yaml mode 0600
	Timeout      time.Duration // per-query default; 0 → DefaultTimeout
	MaxPoolSize  int           // 0 → driver default (50)
}

// BoltClient is the live implementation of Client. It holds a single
// DriverWithContext and hands out short-lived sessions on each call
// to keep transaction lifetimes small and predictable.
type BoltClient struct {
	driver  neodrv.DriverWithContext
	timeout time.Duration
}

// NewBoltClient builds a BoltClient against the given config. The
// driver connection is not established eagerly — neo4j-go-driver v5
// defers TCP connection until the first session. Callers who want to
// fail fast on an unreachable service should call Ping immediately
// after construction.
func NewBoltClient(cfg Config) (*BoltClient, error) {
	endpoint := normalizeBoltURL(cfg.BoltEndpoint)
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}

	drv, err := neodrv.NewDriverWithContext(
		endpoint,
		neodrv.BasicAuth(cfg.Username, cfg.Password, ""),
		func(c *neodrv.Config) {
			if cfg.MaxPoolSize > 0 {
				c.MaxConnectionPoolSize = cfg.MaxPoolSize
			}
		},
	)
	if err != nil {
		return nil, fmt.Errorf("neo4j: new driver: %w", err)
	}
	return &BoltClient{driver: drv, timeout: cfg.Timeout}, nil
}

// normalizeBoltURL accepts either a bare host:port or a scheme-qualified
// URL. Bolt on loopback defaults to the unencrypted `bolt://` scheme
// because Phase 1 services bind to 127.0.0.1 only.
func normalizeBoltURL(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	for _, prefix := range []string{"bolt://", "bolt+s://", "bolt+ssc://", "neo4j://", "neo4j+s://", "neo4j+ssc://"} {
		if strings.HasPrefix(endpoint, prefix) {
			return endpoint
		}
	}
	return "bolt://" + endpoint
}

// Close releases the driver's connection pool.
func (c *BoltClient) Close(ctx context.Context) error {
	return c.driver.Close(ctx)
}

// sessionRunner is the live-driver implementation of cypherRunner.
// It is created per call so session lifetimes stay short.
type sessionRunner struct {
	sess neodrv.SessionWithContext
}

func (s *sessionRunner) Run(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	result, err := s.sess.Run(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	records, err := result.Collect(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		row := make(map[string]any, len(rec.Keys))
		for i, k := range rec.Keys {
			row[k] = rec.Values[i]
		}
		out = append(out, row)
	}
	return out, nil
}

// cypherRunner is the thin seam the adapter uses to talk to a Neo4j
// session. BoltClient methods construct a sessionRunner for each call;
// unit tests inject a fakeRunner instead, avoiding the need to spin
// up a live Neo4j for anything but the integration-tagged suite.
type cypherRunner interface {
	Run(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error)
}

// runWithSession opens a fresh session, wraps it in a sessionRunner,
// and calls fn. It ensures the session is closed on every code path.
func (c *BoltClient) runWithSession(ctx context.Context, access neodrv.AccessMode, fn func(cypherRunner) error) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	sess := c.driver.NewSession(ctx, neodrv.SessionConfig{AccessMode: access})
	defer sess.Close(ctx)
	return fn(&sessionRunner{sess: sess})
}

// Ping runs RETURN 1 and validates the result contains the expected row.
// It uses a read session so it does not contend with active write
// transactions.
func (c *BoltClient) Ping(ctx context.Context) error {
	return c.runWithSession(ctx, neodrv.AccessModeRead, func(r cypherRunner) error {
		rows, err := r.Run(ctx, "RETURN 1 AS n", nil)
		if err != nil {
			return fmt.Errorf("neo4j: ping: %w", err)
		}
		if len(rows) == 0 {
			return errors.New("neo4j: ping returned no rows")
		}
		v, ok := rows[0]["n"]
		if !ok {
			return errors.New("neo4j: ping missing 'n' column")
		}
		// The driver maps a Cypher integer literal to int64.
		if n, ok := v.(int64); !ok || n != 1 {
			return fmt.Errorf("neo4j: ping returned %v, want 1", v)
		}
		return nil
	})
}

// ProbeProcedures runs SHOW PROCEDURES YIELD name WHERE name IN […]
// and returns the availability of each of the GDS procedures Cortex
// cares about. If SHOW PROCEDURES itself fails (e.g., older Neo4j,
// permission denied), we return the error so the caller can surface
// it at cortex up time as a hard readiness failure.
func (c *BoltClient) ProbeProcedures(ctx context.Context) (ProcedureAvailability, error) {
	var out ProcedureAvailability
	err := c.runWithSession(ctx, neodrv.AccessModeRead, func(r cypherRunner) error {
		avail, err := probeProcedures(ctx, r)
		if err != nil {
			return err
		}
		out = avail
		return nil
	})
	return out, err
}

// probeProcedures is the runner-free core so unit tests can exercise
// the availability logic against a fakeRunner.
func probeProcedures(ctx context.Context, r cypherRunner) (ProcedureAvailability, error) {
	rows, err := r.Run(ctx,
		`SHOW PROCEDURES YIELD name WHERE name IN $names RETURN name`,
		map[string]any{
			"names": []string{
				ProcPageRankStream,
				ProcLeidenStream,
				ProcLouvainStream,
			},
		},
	)
	if err != nil {
		return ProcedureAvailability{}, fmt.Errorf("neo4j: probe procedures: %w", err)
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if name, ok := row["name"].(string); ok {
			seen[name] = true
		}
	}
	out := ProcedureAvailability{
		PageRankStream: seen[ProcPageRankStream],
		LeidenStream:   seen[ProcLeidenStream],
		LouvainStream:  seen[ProcLouvainStream],
	}
	// FR-028 convenience: Leiden preferred, Louvain fallback.
	if !out.LeidenStream && out.LouvainStream {
		out.LeidenUnavailable = true
	}
	return out, nil
}

// WriteEntries runs cypher in a write session. Callers are
// responsible for shaping the Cypher (e.g., MERGE vs CREATE) and for
// passing params that serialise cleanly to Bolt types.
func (c *BoltClient) WriteEntries(ctx context.Context, cypher string, params map[string]any) error {
	return c.runWithSession(ctx, neodrv.AccessModeWrite, func(r cypherRunner) error {
		_, err := r.Run(ctx, cypher, params)
		return err
	})
}

// QueryGraph runs a read-only Cypher statement and materialises the
// result set.
func (c *BoltClient) QueryGraph(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	var rows []map[string]any
	err := c.runWithSession(ctx, neodrv.AccessModeRead, func(r cypherRunner) error {
		var rerr error
		rows, rerr = r.Run(ctx, cypher, params)
		return rerr
	})
	return rows, err
}

// RunGDS runs a GDS procedure call. It uses a read session because
// all GDS procedures Cortex calls in Phase 1 (pageRank.stream,
// leiden.stream, louvain.stream) are read-only — they stream results
// without mutating the graph.
func (c *BoltClient) RunGDS(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	var rows []map[string]any
	err := c.runWithSession(ctx, neodrv.AccessModeRead, func(r cypherRunner) error {
		var rerr error
		rows, rerr = r.Run(ctx, cypher, params)
		return rerr
	})
	return rows, err
}
