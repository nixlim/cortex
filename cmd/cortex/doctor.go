// Command cortex — `cortex doctor` wire-up.
//
// This file binds the `cortex doctor` cobra command to the
// orchestration in internal/infra. It assembles the default check
// set against live adapters (Docker, Weaviate, Neo4j, Ollama) and a
// real HostProvider implementation backed by syscall / sysctl /
// net.Listen, then delegates to infra.RunDoctor for execution and
// infra.DoctorReport for the exit-code decision.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Host Prerequisites"
//   docs/spec/cortex-spec.md SC-005 (doctor --json + doctor --quick budgets)
//   docs/spec/cortex-spec.md FR-062 (doctor enforces host prerequisites)
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/weaviate"
	"github.com/spf13/cobra"
)

// runDoctor is the RunE for `cortex doctor`. Flags (quick/full/json)
// are captured in commands.go newDoctorCmd() and forwarded here.
func runDoctor(cmd *cobra.Command, _ []string, quick, full, jsonOut bool) error {
	// --full is the default; --quick flips the mode. We accept both
	// flags because the spec names them both; specifying --full alone
	// is equivalent to the default.
	_ = full
	home, err := os.UserHomeDir()
	if err != nil {
		return errs.Operational("HOME_NOT_FOUND",
			"cannot locate user home directory", err)
	}
	configPath := filepath.Join(home, ".cortex", "config.yaml")
	cfg, err := config.Load(configPath)
	if err != nil {
		return errs.Operational("CONFIG_LOAD_FAILED",
			"failed to load cortex configuration", err)
	}
	cortexHome := filepath.Join(home, ".cortex")

	// Build the live adapters. As with status, neo4j client
	// construction failure is not a hard abort: doctor is a reporting
	// tool and a missing neo4j client is reported as neo4j.ready fail.
	weaviateClient := weaviate.NewHTTPClient(cfg.Endpoints.WeaviateHTTP, 1*time.Second)

	password, _, _ := infra.EnsureNeo4jPassword(configPath)
	neoClient, _ := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      1 * time.Second,
		MaxPoolSize:  2,
	})
	if neoClient != nil {
		defer neoClient.Close(cmd.Context())
	}

	ollamaClient := ollama.NewHTTPClient(ollama.Config{
		Endpoint:              cfg.Endpoints.Ollama,
		EmbeddingModel:        defaultEmbeddingModel,
		GenerationModel:       defaultGenerationModel,
		EmbeddingTimeout:      1 * time.Second,
		LinkDerivationTimeout: 1 * time.Second,
	})

	host := sysHost{}

	checks := []infra.DoctorCheck{
		infra.HostRAMCheck(host),
		infra.HostDiskCheck(host, cortexHome),
		infra.HostFDLimitCheck(host),
		infra.HostPortsCheck(host),
		infra.DockerReadyCheck(infra.ExecDocker{}),
		infra.WeaviateReadyCheck(weaviateStatusAdapter{c: weaviateClient, endpoint: cfg.Endpoints.WeaviateHTTP}),
		infra.Neo4jReadyCheck(neo4jStatusAdapter{c: neoClient}),
		infra.OllamaModelCheck(
			ollamaAdapter{c: ollamaClient, endpoint: cfg.Endpoints.Ollama},
			defaultEmbeddingModel,
			defaultGenerationModel,
		),
		infra.LogQuarantineCheck(cortexHome),
		infra.FilePermissionsCheck(cortexHome),
	}

	report := infra.RunDoctor(cmd.Context(), infra.DoctorOptions{
		Checks: checks,
		Quick:  quick,
	})

	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderHumanDoctor(cmd, report)
	}

	if report.HasFailures() {
		// Return an *errs.Error so main.go's exit-code translation
		// picks exit 1. The message is intentionally terse because
		// the detailed per-check output is already on stdout.
		return errs.Operational("DOCTOR_CHECKS_FAILED",
			fmt.Sprintf("%d doctor check(s) failed", report.Fails), nil)
	}
	return nil
}

// renderHumanDoctor writes a column-aligned summary of the doctor
// report suitable for operators reading a terminal. The JSON shape is
// the source of truth; this is a friendlier projection of the same
// data.
func renderHumanDoctor(cmd *cobra.Command, r infra.DoctorReport) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "cortex doctor  (mode=%s, %d ms, parallelism=%d)\n",
		r.Mode, r.ElapsedMS, r.Parallelism)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "  %-18s  %-4s  %4d ms  %s\n",
			c.Name, c.Status, c.DurationMS, c.Message)
		if c.Status != infra.CheckPass && c.Remediation != "" {
			fmt.Fprintf(w, "                                      remediate: %s\n", c.Remediation)
		}
	}
	fmt.Fprintf(w, "  -> %d pass, %d warn, %d fail\n", r.Passes, r.Warns, r.Fails)
}

// ---------------------------------------------------------------------------
// sysHost is the production HostProvider. It uses syscall.Sysctl
// equivalents on darwin (the only platform Phase 1 targets) and
// net.Listen for port probing.
// ---------------------------------------------------------------------------

type sysHost struct{}

// TotalRAMBytes reads the darwin sysctl value hw.memsize. Phase 1
// targets darwin/arm64 only per the Host Prerequisites table.
func (sysHost) TotalRAMBytes() (uint64, error) {
	// syscall.Sysctl returns the raw bytes; hw.memsize is a little-
	// endian uint64 in those bytes.
	b, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	if len(b) < 8 {
		// Some platforms return the value as a string — fall through
		// to strconv as a defensive alternative path.
		if v, perr := strconv.ParseUint(string(b), 10, 64); perr == nil {
			return v, nil
		}
		return 0, fmt.Errorf("sysctl hw.memsize: short buffer (%d bytes)", len(b))
	}
	// Little-endian assembly. We cannot import encoding/binary purely
	// to decode 8 bytes because the import bloat is not justified;
	// the shift chain below is equivalent and trivially auditable.
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56, nil
}

// FreeDiskBytes reads free bytes on the volume containing path using
// syscall.Statfs_t.
func (sysHost) FreeDiskBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// FDLimit returns the current process's soft file-descriptor limit.
func (sysHost) FDLimit() (uint64, error) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, fmt.Errorf("getrlimit: %w", err)
	}
	return uint64(r.Cur), nil
}

// PortInUse reports whether TCP port on 127.0.0.1 is already bound.
// We probe by briefly attempting to listen on the port; if the listen
// succeeds the port is free and we immediately close the listener. A
// "bind: address already in use" error means the port is bound.
func (sysHost) PortInUse(port int) (bool, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		// Heuristic: treat any listen error as "port busy". This
		// yields false positives in pathological cases (raw-socket
		// permission failures) but never yields false negatives
		// which is what matters for the readiness gate.
		return true, nil
	}
	_ = l.Close()
	return false, nil
}
