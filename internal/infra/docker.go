// Package infra implements the orchestration layer behind `cortex up`,
// `cortex down`, `cortex status`, and `cortex doctor`. It does not own
// any adapters; it composes them behind narrow interfaces so that the
// lifecycle commands can be unit-tested without real Docker, Weaviate,
// Neo4j, or Ollama processes.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Infrastructure Topology"
//   docs/spec/cortex-spec.md §"cortex up Readiness Contract"
//   docs/spec/cortex-spec.md §"Volume Topology and Persistence"
package infra

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// DockerRunner is the test seam for every docker-CLI invocation cortex's
// lifecycle commands need. Production code uses ExecDocker; tests inject
// fakes that record calls and return canned errors.
type DockerRunner interface {
	// Ping returns nil if the docker daemon is reachable. It corresponds
	// to the "Docker daemon reachable" step of the readiness contract.
	Ping(ctx context.Context) error

	// ComposeUp brings up the managed stack in detached mode. env is
	// merged into the subprocess environment on top of os.Environ so the
	// compose file's ${VAR} placeholders (notably NEO4J_PASSWORD) expand
	// to the values the caller just wrote to ~/.cortex/config.yaml.
	ComposeUp(ctx context.Context, composeFile string, env map[string]string) error

	// ComposeDown stops the stack. When removeVolumes is true, named
	// volumes are removed as well ("cortex down --purge").
	ComposeDown(ctx context.Context, composeFile string, removeVolumes bool) error

	// ImageExists reports whether a local image tag is present in the
	// daemon's image store, used by the "build cortex/neo4j-gds locally
	// on first run" pathway.
	ImageExists(ctx context.Context, tag string) (bool, error)

	// Build runs `docker build` against a context directory and tags
	// the result. Used to build the custom neo4j-gds image.
	Build(ctx context.Context, contextDir, tag, dockerfile string) error
}

// ExecDocker is the production DockerRunner that shells out to the
// docker CLI. It is intentionally a value type (no state) so callers
// can pass ExecDocker{} directly.
type ExecDocker struct{}

// Ping runs `docker version --format {{.Server.Version}}` which fails
// fast (non-zero exit) when the daemon is unreachable.
func (ExecDocker) Ping(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker daemon unreachable: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// ComposeUp runs `docker compose -f <file> up -d`. Any entries in env
// are appended to os.Environ() before exec so the compose file's
// ${VAR:-default} placeholders expand to the caller-supplied values
// rather than the literal fallback burned into the image on first boot.
func (ExecDocker) ComposeUp(ctx context.Context, composeFile string, env map[string]string) error {
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "up", "-d")
	if len(env) > 0 {
		merged := os.Environ()
		for k, v := range env {
			merged = append(merged, k+"="+v)
		}
		cmd.Env = merged
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker compose up: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// ComposeDown runs `docker compose -f <file> down`, optionally passing
// `-v` to purge named volumes.
func (ExecDocker) ComposeDown(ctx context.Context, composeFile string, removeVolumes bool) error {
	args := []string{"compose", "-f", composeFile, "down"}
	if removeVolumes {
		args = append(args, "-v")
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker compose down: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// ImageExists consults `docker images -q <tag>` and reports whether the
// daemon already has the tag in its local image store.
func (ExecDocker) ImageExists(ctx context.Context, tag string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "images", "-q", tag)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("docker images: %w", err)
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// Build runs `docker build -t <tag> -f <dockerfile> <contextDir>`.
func (ExecDocker) Build(ctx context.Context, contextDir, tag, dockerfile string) error {
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, "-f", dockerfile, contextDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker build %s: %w: %s", tag, err, bytes.TrimSpace(out))
	}
	return nil
}
