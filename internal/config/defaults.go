// Package config loads and validates the Cortex configuration file.
//
// The canonical file lives at ~/.cortex/config.yaml. Every field from the
// "Configuration Defaults" section of docs/spec/cortex-spec.md is reachable
// as a typed Go field. Defaults mirror the spec exactly; Load() on a missing
// file returns a Config populated with these defaults and no error.
package config

// Config is the root configuration struct.
type Config struct {
	Retrieval          RetrievalConfig          `yaml:"retrieval"`
	Pagination         PaginationConfig         `yaml:"pagination"`
	LinkDerivation     LinkDerivationConfig     `yaml:"link_derivation"`
	Reflection         ReflectionConfig         `yaml:"reflection"`
	Analysis           AnalysisConfig           `yaml:"analysis"`
	CommunityDetection CommunityDetectionConfig `yaml:"community_detection"`
	Ingest             IngestConfig             `yaml:"ingest"`
	Log                LogConfig                `yaml:"log"`
	Doctor             DoctorConfig             `yaml:"doctor"`
	Security           SecurityConfig           `yaml:"security"`
	Migration          MigrationConfig          `yaml:"migration"`
	Timeouts           TimeoutsConfig           `yaml:"timeouts"`
	CLI                CLIConfig                `yaml:"cli"`
	Endpoints          EndpointsConfig          `yaml:"endpoints"`
	OpsLog             OpsLogConfig             `yaml:"ops_log"`
	Disk               DiskConfig               `yaml:"disk"`
	Docker             DockerConfig             `yaml:"docker"`
	Ollama             OllamaConfig             `yaml:"ollama"`
}

// OllamaConfig holds adapter-level knobs for the Ollama HTTP client
// that are not covered by Timeouts or Endpoints. See bead cortex-w5u
// for the rationale behind exposing num_ctx: Ollama defaults to 2048
// which silently truncates trail summaries, link-derivation prompts,
// and any other Generate call with a non-trivial context payload.
type OllamaConfig struct {
	// NumCtx is the Ollama context window (options.num_ctx) passed on
	// every /api/generate request. Zero means "inherit Ollama's own
	// default" (2048 today); an explicit value overrides it per-call.
	// The cortex default is 32768 — Qwen3-4B's full native context
	// window. This is needed by the ingest summarizer, which feeds
	// whole module source bodies (potentially ~100KB of code) to the
	// model and requires the resulting JSON-schema-structured output
	// to quote identifiers, constants, and algorithm names verbatim
	// from the source. At 4-8B q4, a 32K KV cache costs roughly 2-3 GB
	// of VRAM, which fits the supported host profile. See cortex-dww.
	NumCtx int `yaml:"num_ctx"`

	// EmbeddingVectorDim is the expected output dimension of the
	// configured embedding model. The write pipeline compares every
	// freshly-produced vector against this value BEFORE handing it
	// to Weaviate so a mismatch surfaces as EMBEDDING_DIM_MISMATCH
	// with a clear "embedder was changed, run cortex rebuild"
	// remediation rather than leaking as a generic schema error from
	// the Weaviate HTTP layer. Zero disables the check. The cortex
	// default is 768 (nomic-embed-text). See cortex-06p.
	EmbeddingVectorDim int `yaml:"embedding_vector_dim"`
}

type RetrievalConfig struct {
	DefaultLimit int              `yaml:"default_limit"`
	PPR          PPRConfig        `yaml:"ppr"`
	Activation   ActivationConfig `yaml:"activation"`
	Forgetting   ForgettingConfig `yaml:"forgetting"`
	// RelevanceFloor is the legacy single-floor similarity gate. It
	// remains a back-compat alias for RelevanceGate.SimFloorStrict;
	// prefer the sub-struct for new configs. See bead cortex-y6g.
	// Zero disables the gate.
	RelevanceFloor float64              `yaml:"relevance_floor"`
	RelevanceGate  RelevanceGateConfig  `yaml:"relevance_gate"`
}

// RelevanceGateConfig holds the layered relevance gate knobs from
// bead cortex-y6g. A candidate survives the gate when
// sim >= SimFloorStrict OR sim >= SimFloorHard - RescueAlpha*ppr,
// and is unconditionally dropped when sim < SimFloorHard.
type RelevanceGateConfig struct {
	SimFloorHard   float64 `yaml:"sim_floor_hard"`
	SimFloorStrict float64 `yaml:"sim_floor_strict"`
	RescueAlpha    float64 `yaml:"rescue_alpha"`
	// Stage 3 composite floor (cortex-2sg). A candidate that survives
	// Stage 1+2a must also clear
	//   GateSimWeight*sim + GatePPRWeight*ppr >= CompositeFloor.
	// CompositeFloor == 0 disables the stage.
	CompositeFloor float64 `yaml:"composite_floor"`
	GateSimWeight  float64 `yaml:"gate_sim_weight"`
	GatePPRWeight  float64 `yaml:"gate_ppr_weight"`
	// Stage 2b quantile-baseline rescue (cortex-5mp). When the per-query
	// candidate set has at least PPRBaselineMinN entries, a borderline
	// sim uses a strict upper-quartile PPR test instead of the Stage 2a
	// Option-1 formula; below that count the gate falls back to
	// Option-1. Zero disables the quantile path entirely.
	PPRBaselineMinN int `yaml:"ppr_baseline_min_n"`
}

type PPRConfig struct {
	SeedTopK      int     `yaml:"seed_top_k"`
	Damping       float64 `yaml:"damping"`
	MaxIterations int     `yaml:"max_iterations"`
}

type ActivationConfig struct {
	DecayExponent float64           `yaml:"decay_exponent"`
	Weights       ActivationWeights `yaml:"weights"`
}

type ActivationWeights struct {
	BaseLevel  float64 `yaml:"base_level"`
	PPR        float64 `yaml:"ppr"`
	Similarity float64 `yaml:"similarity"`
	Importance float64 `yaml:"importance"`
}

type ForgettingConfig struct {
	VisibilityThreshold float64 `yaml:"visibility_threshold"`
}

type PaginationConfig struct {
	HumanDefaultLimit int `yaml:"human_default_limit"`
	JSONDefaultLimit  int `yaml:"json_default_limit"`
}

type LinkDerivationConfig struct {
	ConfidenceFloor       float64 `yaml:"confidence_floor"`
	SimilarToCosineFloor  float64 `yaml:"similar_to_cosine_floor"`
}

type ReflectionConfig struct {
	MinClusterSize         int     `yaml:"min_cluster_size"`
	MinDistinctTimestamps  int     `yaml:"min_distinct_timestamps"`
	AvgPairwiseCosineFloor float64 `yaml:"avg_pairwise_cosine_floor"`
	MDLCompressionRatio    float64 `yaml:"mdl_compression_ratio"`
}

type AnalysisConfig struct {
	MDLCompressionRatio           float64 `yaml:"mdl_compression_ratio"`
	CrossProjectMinProjects       int     `yaml:"cross_project_min_projects"`
	CrossProjectMaxSharePerProject float64 `yaml:"cross_project_max_share_per_project"`
	CrossProjectImportanceBoost   float64 `yaml:"cross_project_importance_boost"`
}

type CommunityDetectionConfig struct {
	Algorithm     string    `yaml:"algorithm"`
	Levels        int       `yaml:"levels"`
	Resolutions   []float64 `yaml:"resolutions"`
	MaxIterations int       `yaml:"max_iterations"`
	Tolerance     float64   `yaml:"tolerance"`
}

type IngestConfig struct {
	ModuleSizeLimitBytes int                   `yaml:"module_size_limit_bytes"`
	OllamaConcurrency    int                   `yaml:"ollama_concurrency"`
	PostIngestReflect    bool                  `yaml:"post_ingest_reflect"`
	PostIngestAnalyze    bool                  `yaml:"post_ingest_analyze"`
	DefaultStrategy      IngestDefaultStrategy `yaml:"default_strategy"`
}

type IngestDefaultStrategy struct {
	Go                   string `yaml:"go"`
	Java                 string `yaml:"java"`
	Kotlin               string `yaml:"kotlin"`
	Python               string `yaml:"python"`
	JavaScriptTypeScript string `yaml:"javascript_typescript"`
	Rust                 string `yaml:"rust"`
	CSharp               string `yaml:"csharp"`
	Ruby                 string `yaml:"ruby"`
	CCpp                 string `yaml:"c_cpp"`
	Fallback             string `yaml:"fallback"`
}

type LogConfig struct {
	LockTimeoutSeconds       int    `yaml:"lock_timeout_seconds"`
	TailValidationWindowBytes int   `yaml:"tail_validation_window_bytes"`
	SegmentMaxSizeMB         int    `yaml:"segment_max_size_mb"`
	SegmentDir               string `yaml:"segment_dir"`
}

type DoctorConfig struct {
	Parallelism         int `yaml:"parallelism"`
	QuickTimeoutSeconds int `yaml:"quick_timeout_seconds"`
}

type SecurityConfig struct {
	Secrets           SecretsConfig `yaml:"secrets"`
	FileModeDirectory int           `yaml:"file_mode_directory"`
	FileModeFiles     int           `yaml:"file_mode_files"`
}

type SecretsConfig struct {
	BuiltinRuleset    string  `yaml:"builtin_ruleset"`
	CustomRulesetPath string  `yaml:"custom_ruleset_path"`
	EntropyThreshold  float64 `yaml:"entropy_threshold"`
}

type MigrationConfig struct {
	ExcludeFromCrossProject bool `yaml:"exclude_from_cross_project"`
}

type TimeoutsConfig struct {
	EmbeddingSeconds         int `yaml:"embedding_seconds"`
	ConceptExtractionSeconds int `yaml:"concept_extraction_seconds"`
	LinkDerivationSeconds    int `yaml:"link_derivation_seconds"`
	TrailSummarySeconds      int `yaml:"trail_summary_seconds"`
	ReflectionSeconds        int `yaml:"reflection_seconds"`
	IngestSummarySeconds     int `yaml:"ingest_summary_seconds"`
}

type CLIConfig struct {
	ExitCode ExitCodeConfig `yaml:"exit_code"`
}

type ExitCodeConfig struct {
	Success     int `yaml:"success"`
	Operational int `yaml:"operational"`
	Validation  int `yaml:"validation"`
}

type EndpointsConfig struct {
	WeaviateHTTP string `yaml:"weaviate_http"`
	WeaviateGRPC string `yaml:"weaviate_grpc"`
	Neo4jBolt    string `yaml:"neo4j_bolt"`
	Ollama       string `yaml:"ollama"`
}

type OpsLogConfig struct {
	Format     string `yaml:"format"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
}

type DiskConfig struct {
	WarningThresholdGB int `yaml:"warning_threshold_gb"`
}

type DockerConfig struct {
	Neo4jGDSDockerfile string `yaml:"neo4j_gds_dockerfile"`
}

// Defaults returns a Config populated from the Configuration Defaults table
// in cortex-spec.md. It is the single source of truth for spec defaults
// and must be kept in sync with the spec.
func Defaults() Config {
	return Config{
		Retrieval: RetrievalConfig{
			DefaultLimit: 10,
			PPR: PPRConfig{
				SeedTopK:      5,
				Damping:       0.85,
				MaxIterations: 20,
			},
			Activation: ActivationConfig{
				DecayExponent: 0.5,
				Weights: ActivationWeights{
					BaseLevel:  0.3,
					PPR:        0.3,
					Similarity: 0.3,
					Importance: 0.1,
				},
			},
			Forgetting: ForgettingConfig{
				// 0.0005 keeps a freshly-encoded entry visible for ~30
				// days under the default decay (1 * (1+age)^-0.5):
				// solve 0.0005 = (1+age)^-0.5  ⟹ age ≈ 4·10^6 s ≈ 46d.
				// The previous 0.05 threshold made unreinforced entries
				// invisible after only ~399s (~6.7 minutes), which broke
				// any cross-session recall and made `cortex ingest` look
				// dead within minutes of finishing. See bead cortex-upp.
				VisibilityThreshold: 0.0005,
			},
			// Legacy single-floor gate. Retained as a back-compat
			// alias for RelevanceGate.SimFloorStrict — cortex-y6g
			// replaces it with the layered gate below but keeps
			// this field populated so older code paths and configs
			// continue to behave identically when relevance_gate
			// is absent.
			RelevanceFloor: 0.55,
			// Layered relevance gate (cortex-y6g). Strict=0.55
			// preserves the old top-of-gate behavior; hard=0.40
			// is well below any observed negative rank-1 sim in
			// deep-eval dump 20260412T225007Z, so a candidate
			// between 0.40 and 0.55 must earn its slot via PPR
			// rescue: sim >= 0.40 - 0.15*ppr.
			RelevanceGate: RelevanceGateConfig{
				SimFloorHard:   0.40,
				SimFloorStrict: 0.55,
				RescueAlpha:    0.15,
				CompositeFloor:  0.45,
				GateSimWeight:   0.7,
				GatePPRWeight:   0.3,
				PPRBaselineMinN: 25,
			},
		},
		Pagination: PaginationConfig{
			HumanDefaultLimit: 20,
			JSONDefaultLimit:  100,
		},
		LinkDerivation: LinkDerivationConfig{
			ConfidenceFloor:      0.60,
			SimilarToCosineFloor: 0.75,
		},
		Reflection: ReflectionConfig{
			MinClusterSize:         3,
			MinDistinctTimestamps:  2,
			AvgPairwiseCosineFloor: 0.65,
			MDLCompressionRatio:    1.3,
		},
		Analysis: AnalysisConfig{
			MDLCompressionRatio:            1.15,
			CrossProjectMinProjects:        2,
			CrossProjectMaxSharePerProject: 0.70,
			CrossProjectImportanceBoost:    0.20,
		},
		CommunityDetection: CommunityDetectionConfig{
			Algorithm:     "leiden",
			Levels:        3,
			Resolutions:   []float64{1.0, 0.5, 0.1},
			MaxIterations: 10,
			Tolerance:     0.0001,
		},
		Ingest: IngestConfig{
			ModuleSizeLimitBytes: 262144,
			// OllamaConcurrency bounds the number of module summary
			// calls in flight against /api/generate. On a constrained
			// local Ollama running qwen3:4b at NumCtx=32768, 4 concurrent
			// calls overflow the model's single-request capacity and 3 of
			// them queue past their per-request deadline. 2 matches what
			// a consumer GPU can actually serve at 32K context. Raise
			// this in config.yaml if your Ollama host has more VRAM or
			// you're using a smaller model. See cortex-8rk.
			OllamaConcurrency:    2,
			PostIngestReflect:    true,
			PostIngestAnalyze:    false,
			DefaultStrategy: IngestDefaultStrategy{
				Go:                   "per-package",
				Java:                 "per-class",
				Kotlin:               "per-file",
				Python:               "per-file",
				JavaScriptTypeScript: "per-file",
				Rust:                 "per-module",
				CSharp:               "per-class",
				Ruby:                 "per-class-or-module",
				CCpp:                 "per-pair",
				Fallback:             "per-file",
			},
		},
		Log: LogConfig{
			LockTimeoutSeconds:        5,
			TailValidationWindowBytes: 65536,
			SegmentMaxSizeMB:          64,
			SegmentDir:                "~/.cortex/log.d",
		},
		Doctor: DoctorConfig{
			Parallelism:         4,
			QuickTimeoutSeconds: 5,
		},
		Security: SecurityConfig{
			Secrets: SecretsConfig{
				BuiltinRuleset:    "embedded",
				CustomRulesetPath: "~/.cortex/secrets.yaml",
				EntropyThreshold:  4.5,
			},
			FileModeDirectory: 0o700,
			FileModeFiles:     0o600,
		},
		Migration: MigrationConfig{
			ExcludeFromCrossProject: true,
		},
		Timeouts: TimeoutsConfig{
			EmbeddingSeconds:         30,
			ConceptExtractionSeconds: 5,
			LinkDerivationSeconds:    60,
			TrailSummarySeconds:      60,
			ReflectionSeconds:        60,
			// IngestSummarySeconds is the per-module wall clock for the
			// ingest summarizer's structured-output call. At NumCtx=32768
			// with a 100KB module prompt on a 4-8B q4 model running
			// locally, a single /api/generate can take several minutes
			// end-to-end (prompt processing + constrained decoding), so
			// the default is generous. Raised to 1800s after observing
			// ~600s timeouts on large Go per-package summaries in the
			// cortex self-ingest on a local qwen3:4b. See cortex-8rk.
			IngestSummarySeconds: 1800,
		},
		Ollama: OllamaConfig{
			NumCtx:             32768,
			EmbeddingVectorDim: 768,
		},
		CLI: CLIConfig{
			ExitCode: ExitCodeConfig{
				Success:     0,
				Operational: 1,
				Validation:  2,
			},
		},
		Endpoints: EndpointsConfig{
			WeaviateHTTP: "localhost:9397",
			WeaviateGRPC: "localhost:50051",
			Neo4jBolt:    "localhost:7687",
			Ollama:       "localhost:11434",
		},
		OpsLog: OpsLogConfig{
			Format:    "jsonl",
			MaxSizeMB: 50,
		},
		Disk: DiskConfig{
			WarningThresholdGB: 1,
		},
	}
}
