package infra

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HostProvider is the test seam for every check that touches the
// operator's machine directly (RAM, ulimit, port sockets, file system
// metadata). cmd/cortex/doctor.go supplies a production implementation
// built on top of sysctl / syscall.Statfs / net.Listen; tests inject a
// table-driven fake.
//
// Every method returns an error only on enumeration failure — a
// missing or out-of-range value is an in-range success, classified by
// the check function itself.
type HostProvider interface {
	// TotalRAMBytes returns the machine's total physical RAM.
	TotalRAMBytes() (uint64, error)

	// FreeDiskBytes returns the number of free bytes on the volume
	// containing path. Used for the ~/.cortex disk-space check.
	FreeDiskBytes(path string) (uint64, error)

	// FDLimit returns the current process's soft file-descriptor
	// limit. Used for the ulimit -n prerequisite.
	FDLimit() (uint64, error)

	// PortInUse reports whether TCP port on 127.0.0.1 is currently
	// bound. A true value means something else already owns the port
	// and cortex up will fail to bind.
	PortInUse(port int) (bool, error)
}

// HostRAMCheck verifies the machine has at least 12 GB of total RAM
// per FR-059 and the Host Prerequisites table. A host under the
// floor fails with HOST_RAM_BELOW_FLOOR and remediation pointing at
// the recommended 16 GB.
func HostRAMCheck(host HostProvider) DoctorCheck {
	return checkFunc{
		name:  "host.ram",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			total, err := host.TotalRAMBytes()
			if err != nil {
				return CheckResult{
					Name:        "host.ram",
					Status:      CheckFail,
					Code:        CodeHostRAMBelowFloor,
					Message:     "could not read host total RAM: " + err.Error(),
					Remediation: "check system sysctl(8) / /proc/meminfo availability",
				}
			}
			if total < MinimumHostRAMBytes {
				return CheckResult{
					Name:   "host.ram",
					Status: CheckFail,
					Code:   CodeHostRAMBelowFloor,
					Message: fmt.Sprintf(
						"host has %d bytes RAM; Cortex requires at least %d (12 GB)",
						total, MinimumHostRAMBytes),
					Remediation: "upgrade to a machine with 16 GB RAM or more",
				}
			}
			return CheckResult{
				Name:    "host.ram",
				Status:  CheckPass,
				Message: fmt.Sprintf("%d bytes (%.1f GB)", total, float64(total)/(1024*1024*1024)),
			}
		},
	}
}

// HostDiskCheck verifies the ~/.cortex volume has at least 10 GB free.
// Warns (not fails) below the recommended 50 GB so operators get
// forward notice but cortex up still proceeds.
func HostDiskCheck(host HostProvider, cortexHome string) DoctorCheck {
	return checkFunc{
		name:  "host.disk",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			free, err := host.FreeDiskBytes(cortexHome)
			if err != nil {
				return CheckResult{
					Name:        "host.disk",
					Status:      CheckFail,
					Code:        CodeHostDiskLow,
					Message:     "could not read free disk on " + cortexHome + ": " + err.Error(),
					Remediation: "verify " + cortexHome + " exists and is mounted",
				}
			}
			const floor = uint64(10) * 1024 * 1024 * 1024 // 10 GB
			const warnAt = uint64(50) * 1024 * 1024 * 1024
			if free < floor {
				return CheckResult{
					Name:        "host.disk",
					Status:      CheckFail,
					Code:        CodeHostDiskLow,
					Message:     fmt.Sprintf("only %d bytes free on %s (floor 10 GB)", free, cortexHome),
					Remediation: "free disk space or move ~/.cortex to a larger volume",
				}
			}
			if free < warnAt {
				return CheckResult{
					Name:        "host.disk",
					Status:      CheckWarn,
					Code:        CodeHostDiskLow,
					Message:     fmt.Sprintf("%d bytes free; recommended 50 GB+", free),
					Remediation: "consider freeing disk space before heavy ingest",
				}
			}
			return CheckResult{
				Name:    "host.disk",
				Status:  CheckPass,
				Message: fmt.Sprintf("%d bytes free", free),
			}
		},
	}
}

// HostFDLimitCheck verifies the current process's soft file-descriptor
// limit is at least 4096 (hard fail under the floor, warn below 8192
// per the Host Prerequisites recommended value).
func HostFDLimitCheck(host HostProvider) DoctorCheck {
	return checkFunc{
		name:  "host.fd_limit",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			n, err := host.FDLimit()
			if err != nil {
				return CheckResult{
					Name:        "host.fd_limit",
					Status:      CheckFail,
					Code:        CodeHostUlimitLow,
					Message:     "could not read file-descriptor limit: " + err.Error(),
					Remediation: "run: ulimit -n 8192",
				}
			}
			if n < 4096 {
				return CheckResult{
					Name:        "host.fd_limit",
					Status:      CheckFail,
					Code:        CodeHostUlimitLow,
					Message:     fmt.Sprintf("ulimit -n is %d; floor is 4096", n),
					Remediation: "run: ulimit -n 8192",
				}
			}
			if n < 8192 {
				return CheckResult{
					Name:        "host.fd_limit",
					Status:      CheckWarn,
					Code:        CodeHostUlimitLow,
					Message:     fmt.Sprintf("ulimit -n is %d; recommended 8192", n),
					Remediation: "run: ulimit -n 8192",
				}
			}
			return CheckResult{
				Name:    "host.fd_limit",
				Status:  CheckPass,
				Message: fmt.Sprintf("ulimit -n = %d", n),
			}
		},
	}
}

// HostPortsCheck probes each well-known Cortex port and reports any
// that are already bound. The list matches the Host Prerequisites
// table: 9397 (Cortex HTTP), 50051 (Cortex gRPC), 7474 (Neo4j HTTP),
// 7687 (Neo4j Bolt).
func HostPortsCheck(host HostProvider) DoctorCheck {
	return checkFunc{
		name:  "host.ports",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			required := []int{9397, 50051, 7474, 7687}
			var busy []int
			for _, p := range required {
				inUse, err := host.PortInUse(p)
				if err != nil {
					return CheckResult{
						Name:        "host.ports",
						Status:      CheckFail,
						Code:        CodeHostPortBusy,
						Message:     fmt.Sprintf("could not probe port %d: %v", p, err),
						Remediation: "inspect with: lsof -iTCP -sTCP:LISTEN",
					}
				}
				if inUse {
					busy = append(busy, p)
				}
			}
			if len(busy) > 0 {
				return CheckResult{
					Name:        "host.ports",
					Status:      CheckFail,
					Code:        CodeHostPortBusy,
					Message:     fmt.Sprintf("required ports already bound: %v", busy),
					Remediation: "stop the processes holding those ports, or change cortex port config",
				}
			}
			return CheckResult{
				Name:    "host.ports",
				Status:  CheckPass,
				Message: fmt.Sprintf("all %d required ports free", len(required)),
			}
		},
	}
}

// FilePermissionsCheck walks cortexHome and verifies directories are
// mode 0700 and files are mode 0600 per §"File Permissions" (line
// 790). A single wrong mode fails the check; the message names the
// first offender so remediation is actionable.
//
// This is a full-mode check: the walk's cost scales with the log
// directory's entry count and can exceed the 500 ms quick budget on
// a large log.
func FilePermissionsCheck(cortexHome string) DoctorCheck {
	return checkFunc{
		name:  "fs.permissions",
		quick: false,
		run: func(ctx context.Context) CheckResult {
			var offender string
			err := filepath.Walk(cortexHome, func(path string, info os.FileInfo, err error) error {
				if err != nil || offender != "" {
					return nil
				}
				mode := info.Mode().Perm()
				if info.IsDir() && mode != 0o700 {
					offender = fmt.Sprintf("%s has mode %o, want 0700", path, mode)
					return filepath.SkipAll
				}
				if info.Mode().IsRegular() && mode != 0o600 {
					offender = fmt.Sprintf("%s has mode %o, want 0600", path, mode)
					return filepath.SkipAll
				}
				return nil
			})
			if err != nil && offender == "" {
				return CheckResult{
					Name:        "fs.permissions",
					Status:      CheckFail,
					Code:        CodeHostPermissionsWrong,
					Message:     "walk failed: " + err.Error(),
					Remediation: "verify " + cortexHome + " exists and is readable",
				}
			}
			if offender != "" {
				return CheckResult{
					Name:        "fs.permissions",
					Status:      CheckFail,
					Code:        CodeHostPermissionsWrong,
					Message:     offender,
					Remediation: "run: chmod -R go-rwx " + cortexHome,
				}
			}
			return CheckResult{
				Name:    "fs.permissions",
				Status:  CheckPass,
				Message: "all entries under " + cortexHome + " have owner-only permissions",
			}
		},
	}
}

// LogQuarantineCheck counts segments under ~/.cortex/log.d/.quarantine/
// and warns (non-fatal) when any are present. The datom log continues
// to load segment files individually per FR-055 — a quarantined
// segment is informative, not a startup blocker, so this stays a warn.
func LogQuarantineCheck(cortexHome string) DoctorCheck {
	return checkFunc{
		name:  "log.quarantine",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			dir := filepath.Join(cortexHome, "log.d", ".quarantine")
			entries, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					return CheckResult{
						Name:    "log.quarantine",
						Status:  CheckPass,
						Message: "no quarantine directory present",
					}
				}
				return CheckResult{
					Name:        "log.quarantine",
					Status:      CheckFail,
					Code:        CodeLogQuarantine,
					Message:     "could not read quarantine directory: " + err.Error(),
					Remediation: "inspect " + dir,
				}
			}
			count := 0
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
					count++
				}
			}
			if count == 0 {
				return CheckResult{
					Name:    "log.quarantine",
					Status:  CheckPass,
					Message: "quarantine directory is empty",
				}
			}
			return CheckResult{
				Name:        "log.quarantine",
				Status:      CheckWarn,
				Code:        CodeLogQuarantine,
				Message:     fmt.Sprintf("%d segment(s) in quarantine", count),
				Remediation: "inspect " + dir + " and review ops.log for checksum failures",
			}
		},
	}
}

// OllamaModelCheck verifies the required Ollama models are installed.
// It reuses the same containsModel helper as the cortex up readiness
// contract so the presence test is identical across the two commands.
// Missing models fail with OLLAMA_MODEL_MISSING + an `ollama pull`
// remediation string per FR-062.
func OllamaModelCheck(ollama OllamaReady, embedding, generation string) DoctorCheck {
	return checkFunc{
		name:  "ollama.models",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			models, err := ollama.ListModels(ctx)
			if err != nil {
				return CheckResult{
					Name:        "ollama.models",
					Status:      CheckFail,
					Code:        CodeOllamaNotReachable,
					Message:     "could not list ollama models: " + err.Error(),
					Remediation: "start ollama: brew services start ollama",
				}
			}
			var missing []string
			if embedding != "" && !containsModel(models, embedding) {
				missing = append(missing, embedding)
			}
			if generation != "" && !containsModel(models, generation) {
				missing = append(missing, generation)
			}
			if len(missing) > 0 {
				return CheckResult{
					Name:   "ollama.models",
					Status: CheckFail,
					Code:   CodeOllamaModelMissing,
					Message: fmt.Sprintf(
						"required ollama model(s) missing: %s",
						strings.Join(missing, ", ")),
					Remediation: "run: ollama pull " + missing[0],
				}
			}
			return CheckResult{
				Name:    "ollama.models",
				Status:  CheckPass,
				Message: fmt.Sprintf("%d model(s) installed, required present", len(models)),
			}
		},
	}
}

// WeaviateReadyCheck reuses the narrow WeaviateStatusProbe from the
// status command so the two commands agree on what "ready" means.
func WeaviateReadyCheck(probe WeaviateStatusProbe) DoctorCheck {
	return checkFunc{
		name:  "weaviate.ready",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			if err := probe.Ready(ctx); err != nil {
				return CheckResult{
					Name:        "weaviate.ready",
					Status:      CheckFail,
					Code:        CodeWeaviateNotReady,
					Message:     shallowErrString(err),
					Remediation: "run: cortex up",
				}
			}
			v, verr := probe.Version(ctx)
			if verr != nil {
				return CheckResult{
					Name:        "weaviate.ready",
					Status:      CheckWarn,
					Code:        CodeWeaviateNotReady,
					Message:     "weaviate ready but /v1/meta failed: " + shallowErrString(verr),
					Remediation: "verify weaviate is fully started",
				}
			}
			return CheckResult{
				Name:    "weaviate.ready",
				Status:  CheckPass,
				Message: "weaviate " + v,
			}
		},
	}
}

// Neo4jReadyCheck reuses the Neo4jStatusProbe so doctor agrees with
// status on the Neo4j readiness classification.
func Neo4jReadyCheck(probe Neo4jStatusProbe) DoctorCheck {
	return checkFunc{
		name:  "neo4j.ready",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			if err := probe.Ping(ctx); err != nil {
				return CheckResult{
					Name:        "neo4j.ready",
					Status:      CheckFail,
					Code:        CodeNeo4jNotReady,
					Message:     shallowErrString(err),
					Remediation: "run: cortex up",
				}
			}
			v, verr := probe.Version(ctx)
			if verr != nil {
				return CheckResult{
					Name:        "neo4j.ready",
					Status:      CheckWarn,
					Code:        CodeNeo4jNotReady,
					Message:     "neo4j ready but version probe failed: " + shallowErrString(verr),
					Remediation: "inspect neo4j logs",
				}
			}
			return CheckResult{
				Name:    "neo4j.ready",
				Status:  CheckPass,
				Message: "neo4j " + v,
			}
		},
	}
}

// DockerReadyCheck reuses DockerRunner.Ping so both up and doctor
// agree on "docker daemon reachable".
func DockerReadyCheck(docker DockerRunner) DoctorCheck {
	return checkFunc{
		name:  "docker.ready",
		quick: true,
		run: func(ctx context.Context) CheckResult {
			if err := docker.Ping(ctx); err != nil {
				return CheckResult{
					Name:        "docker.ready",
					Status:      CheckFail,
					Code:        CodeDockerUnreachable,
					Message:     shallowErrString(err),
					Remediation: "start Docker Desktop (or the docker daemon) and retry",
				}
			}
			return CheckResult{
				Name:    "docker.ready",
				Status:  CheckPass,
				Message: "docker daemon reachable",
			}
		},
	}
}
