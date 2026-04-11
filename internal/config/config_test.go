package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load on missing file should succeed: %v", err)
	}
	want := Defaults()
	if cfg.Retrieval.DefaultLimit != want.Retrieval.DefaultLimit {
		t.Errorf("defaults not populated")
	}
	if cfg.Log.SegmentMaxSizeMB != 64 {
		t.Errorf("segment_max_size_mb: got %d want 64", cfg.Log.SegmentMaxSizeMB)
	}
}

func TestLoadInsecurePermissionsReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("retrieval:\n  default_limit: 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("expected ErrInsecurePermissions, got %v", err)
	}
}

func TestLoadWithOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
retrieval:
  default_limit: 25
  activation:
    decay_exponent: 0.7
log:
  segment_max_size_mb: 128
endpoints:
  ollama: "localhost:9999"
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retrieval.DefaultLimit != 25 {
		t.Errorf("default_limit override failed: %d", cfg.Retrieval.DefaultLimit)
	}
	if cfg.Retrieval.Activation.DecayExponent != 0.7 {
		t.Errorf("decay override failed: %v", cfg.Retrieval.Activation.DecayExponent)
	}
	if cfg.Log.SegmentMaxSizeMB != 128 {
		t.Errorf("segment size override failed: %d", cfg.Log.SegmentMaxSizeMB)
	}
	if cfg.Endpoints.Ollama != "localhost:9999" {
		t.Errorf("endpoint override failed: %s", cfg.Endpoints.Ollama)
	}
	// Defaults preserved where not overridden:
	if cfg.Pagination.JSONDefaultLimit != 100 {
		t.Errorf("json default not preserved: %d", cfg.Pagination.JSONDefaultLimit)
	}
}

func TestLoadMalformedYAMLNamesOffendingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// decay_exponent must be float; string triggers TypeError with key.
	yaml := "retrieval:\n  activation:\n    decay_exponent: \"not-a-number\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected malformed-yaml error")
	}
	var me *MalformedError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MalformedError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "decay_exponent") {
		t.Errorf("error should name offending key 'decay_exponent': %v", err)
	}
}

func TestDefaultsCompleteness(t *testing.T) {
	// Guardrail: spot-check every top-level section from the spec is
	// reachable as a populated field on Defaults(). If a new section is
	// added to spec, add it here to keep this test load-bearing.
	d := Defaults()
	if d.Retrieval.PPR.Damping != 0.85 {
		t.Error("retrieval.ppr.damping default wrong")
	}
	if d.Retrieval.Forgetting.VisibilityThreshold != 0.0005 {
		t.Error("visibility_threshold default wrong")
	}
	if d.LinkDerivation.ConfidenceFloor != 0.60 {
		t.Error("link_derivation.confidence_floor default wrong")
	}
	if d.Reflection.MinDistinctTimestamps != 2 {
		t.Error("reflection.min_distinct_timestamps default wrong")
	}
	if d.Analysis.CrossProjectMaxSharePerProject != 0.70 {
		t.Error("analysis.cross_project_max_share_per_project default wrong")
	}
	if d.CommunityDetection.Algorithm != "leiden" {
		t.Error("community_detection.algorithm default wrong")
	}
	if len(d.CommunityDetection.Resolutions) != 3 {
		t.Error("community resolutions default wrong")
	}
	if d.Ingest.DefaultStrategy.Go != "per-package" {
		t.Error("ingest.default_strategy.go default wrong")
	}
	if d.Ingest.DefaultStrategy.CCpp != "per-pair" {
		t.Error("ingest.default_strategy.c_cpp default wrong")
	}
	if d.Log.LockTimeoutSeconds != 5 {
		t.Error("log.lock_timeout_seconds default wrong")
	}
	if d.Log.SegmentDir != "~/.cortex/log.d" {
		t.Error("log.segment_dir default wrong")
	}
	if d.Doctor.Parallelism != 4 {
		t.Error("doctor.parallelism default wrong")
	}
	if d.Security.Secrets.EntropyThreshold != 4.5 {
		t.Error("security.secrets.entropy_threshold default wrong")
	}
	if d.Security.FileModeFiles != 0o600 {
		t.Error("security.file_mode_files default wrong")
	}
	if d.Migration.ExcludeFromCrossProject != true {
		t.Error("migration.exclude_from_cross_project default wrong")
	}
	if d.Timeouts.IngestSummarySeconds != 600 {
		t.Error("timeouts.ingest_summary_seconds default wrong")
	}
	if d.Ingest.OllamaConcurrency != 2 {
		t.Error("ingest.ollama_concurrency default wrong")
	}
	if d.CLI.ExitCode.Validation != 2 {
		t.Error("cli.exit_code.validation default wrong")
	}
	if d.Endpoints.WeaviateHTTP != "localhost:9397" {
		t.Error("endpoints.weaviate_http default wrong")
	}
	if d.OpsLog.MaxSizeMB != 50 {
		t.Error("ops_log.max_size_mb default wrong")
	}
	if d.Disk.WarningThresholdGB != 1 {
		t.Error("disk.warning_threshold_gb default wrong")
	}
}
