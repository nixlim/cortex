// Deep eval data collector. Reads eval/deep/questions_deep.json, runs
// cortex recall for each question (and every rephrasing), captures
// verbatim bodies and scores, computes deterministic metrics, and
// writes one dump file to eval/deep/runs/<timestamp>.json.
//
// No subjective scoring is performed here; that is done by an agent
// that reads the dump and produces <timestamp>.scored.json (see
// eval/deep/README.md and eval/deep/schema/scored.schema.json).
//
// Contract knobs pinned by this runner (recorded in the dump header so
// historical runs stay interpretable):
//
//   - RecallK: --limit passed to cortex recall (default 5).
//   - NegativeScoreThreshold: a question with should_be_empty=true is
//     considered triggered if len(Results)==0 OR Results[0].Score is
//     below this value. Default 0.05 — matches the spec default for
//     retrieval.forgetting.visibility_threshold.
//   - Module granularity: the "module" of a result is parsed from the
//     first line of Body using the regex `per-file:(\S+)`. Module
//     matches and rephrasing-agreement are computed at this granularity
//     (not at EntryID level), so two chunks of the same file count as
//     the same module — see README.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type question struct {
	ID              int      `json:"id"`
	Q               string   `json:"q"`
	Intent          string   `json:"intent"`
	ExpectedModules []string `json:"expected_modules"`
	GoldAnswer      string   `json:"gold_answer"`
	Rephrasings     []string `json:"rephrasings"`
	ShouldBeEmpty   bool     `json:"should_be_empty"`
}

// recallEnvelope matches the JSON shape written by cmd/cortex/recall.go
// (renderRecallResult with --json). We only decode the fields we use.
type recallEnvelope struct {
	Results []recallResult `json:"Results"`
}

type recallResult struct {
	EntryID        string  `json:"EntryID"`
	Body           string  `json:"Body"`
	Score          float64 `json:"Score"`
	BaseActivation float64 `json:"BaseActivation"`
	PPRScore       float64 `json:"PPRScore"`
	Similarity     float64 `json:"Similarity"`
	Importance     float64 `json:"Importance"`
	WhySurfaced    []string `json:"WhySurfaced"`
}

// capturedHit is the per-result record we persist. Body is kept
// verbatim so the downstream agent can judge without re-querying.
type capturedHit struct {
	Rank           int      `json:"rank"`
	EntryID        string   `json:"entry_id"`
	Module         string   `json:"module"`
	Score          float64  `json:"score"`
	BaseActivation float64  `json:"base_activation"`
	PPRScore       float64  `json:"ppr_score"`
	Similarity     float64  `json:"similarity"`
	Importance     float64  `json:"importance"`
	WhySurfaced    []string `json:"why_surfaced"`
	Body           string   `json:"body"`
}

type retrievalRun struct {
	Query      string        `json:"query"`
	IsRephrase bool          `json:"is_rephrasing"`
	Err        string        `json:"error,omitempty"`
	Hits       []capturedHit `json:"hits"`
}

type metrics struct {
	PAt1                 float64 `json:"p_at_1"`
	PAt3                 float64 `json:"p_at_3"`
	MRR                  float64 `json:"mrr"`
	RephrasingAgreement  float64 `json:"rephrasing_agreement"`
	NegativeTriggered    *bool   `json:"negative_triggered,omitempty"`
}

type questionRecord struct {
	question
	Retrievals []retrievalRun `json:"retrievals"`
	Metrics    metrics        `json:"metrics"`
}

type runHeader struct {
	Timestamp             string `json:"timestamp"`
	Commit                string `json:"commit"`
	CortexBinary          string `json:"cortex_binary"`
	QuestionsFile         string `json:"questions_file"`
	RecallK               int    `json:"recall_k"`
	NegativeScoreThreshold float64 `json:"negative_score_threshold"`
	ModuleRegex           string `json:"module_regex"`
	ModuleGranularity     string `json:"module_granularity"`
	Runner                string `json:"runner"`
}

type dump struct {
	Header   runHeader        `json:"header"`
	Summary  map[string]any   `json:"summary"`
	Records  []questionRecord `json:"records"`
}

// modulePathRe extracts the module path from the first line of a body.
// Ingest writes this line as `Module <lang>:per-<strategy>:<path> (<lang>).`
// where strategy is one of per-file, per-package, per-class, per-module,
// per-pair, per-class-or-module. We accept any per-<strategy>:<path>.
var modulePathRe = regexp.MustCompile(`per-[a-z-]+:(\S+?)\s*\(`)

func main() {
	var (
		questionsPath string
		outDir        string
		cortexBin     string
		recallK       int
		negThreshold  float64
		timeoutSec    int
	)
	flag.StringVar(&questionsPath, "questions", "eval/deep/questions_deep.json", "path to questions file")
	flag.StringVar(&outDir, "out", "eval/deep/runs", "output directory for dump files")
	flag.StringVar(&cortexBin, "cortex", "./cortex", "path to cortex binary")
	flag.IntVar(&recallK, "k", 5, "--limit passed to cortex recall")
	flag.Float64Var(&negThreshold, "neg-threshold", 0.05, "should_be_empty trigger: top1.score < this counts as empty")
	flag.IntVar(&timeoutSec, "timeout", 60, "per-query timeout seconds")
	flag.Parse()

	if _, err := os.Stat(cortexBin); err != nil {
		fmt.Fprintf(os.Stderr, "cortex binary not found at %s (build with: go build -o cortex ./cmd/cortex)\n", cortexBin)
		os.Exit(2)
	}

	qs, err := loadQuestions(questionsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	commit := gitHead()
	ts := time.Now().UTC().Format("20060102T150405Z")
	outPath := filepath.Join(outDir, ts+".json")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	header := runHeader{
		Timestamp:              ts,
		Commit:                 commit,
		CortexBinary:           cortexBin,
		QuestionsFile:          questionsPath,
		RecallK:                recallK,
		NegativeScoreThreshold: negThreshold,
		ModuleRegex:            modulePathRe.String(),
		ModuleGranularity:      "file (per-file:<path>)",
		Runner:                 "eval/deep/runner (go)",
	}

	records := make([]questionRecord, 0, len(qs))
	var pass1, pass3, rephraseOK, negOK, totalScored int
	var mrrSum float64
	var negTotal, rephraseTotal int

	for _, q := range qs {
		rec := questionRecord{question: q}
		rec.Retrievals = append(rec.Retrievals, runRecall(cortexBin, q.Q, false, recallK, timeoutSec))
		for _, rp := range q.Rephrasings {
			rec.Retrievals = append(rec.Retrievals, runRecall(cortexBin, rp, true, recallK, timeoutSec))
		}

		rec.Metrics = computeMetrics(q, rec.Retrievals, negThreshold)

		if !q.ShouldBeEmpty && len(q.ExpectedModules) > 0 {
			totalScored++
			if rec.Metrics.PAt1 == 1 {
				pass1++
			}
			if rec.Metrics.PAt3 == 1 {
				pass3++
			}
			mrrSum += rec.Metrics.MRR
		}
		if len(q.Rephrasings) > 0 && !q.ShouldBeEmpty {
			rephraseTotal++
			if rec.Metrics.RephrasingAgreement == 1 {
				rephraseOK++
			}
		}
		if q.ShouldBeEmpty {
			negTotal++
			if rec.Metrics.NegativeTriggered != nil && *rec.Metrics.NegativeTriggered {
				negOK++
			}
		}

		records = append(records, rec)
		fmt.Fprintf(os.Stderr, "[%d/%d] id=%d intent=%s p@1=%.0f p@3=%.0f mrr=%.2f\n",
			len(records), len(qs), q.ID, q.Intent, rec.Metrics.PAt1, rec.Metrics.PAt3, rec.Metrics.MRR)
	}

	summary := map[string]any{
		"questions":            len(qs),
		"scored_positive":      totalScored,
		"p_at_1":               ratio(pass1, totalScored),
		"p_at_3":               ratio(pass3, totalScored),
		"mrr":                  ratioF(mrrSum, float64(totalScored)),
		"rephrasing_agreement": ratio(rephraseOK, rephraseTotal),
		"negative_triggered":   ratio(negOK, negTotal),
	}

	d := dump{Header: header, Summary: summary, Records: records}
	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = f.Close()

	fmt.Fprintf(os.Stderr, "\nwrote %s\n", outPath)
	fmt.Fprintf(os.Stderr, "summary: P@1=%.2f P@3=%.2f MRR=%.2f rephrase=%.2f neg=%.2f\n",
		summary["p_at_1"], summary["p_at_3"], summary["mrr"],
		summary["rephrasing_agreement"], summary["negative_triggered"])
}

func loadQuestions(path string) ([]question, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read questions: %w", err)
	}
	var qs []question
	if err := json.Unmarshal(b, &qs); err != nil {
		return nil, fmt.Errorf("parse questions: %w", err)
	}
	sort.Slice(qs, func(i, j int) bool { return qs[i].ID < qs[j].ID })
	return qs, nil
}

func gitHead() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func runRecall(bin, query string, isRephrase bool, k, timeoutSec int) retrievalRun {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "recall", query, "--limit", fmt.Sprint(k), "--json")
	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	run := retrievalRun{Query: query, IsRephrase: isRephrase}
	if err := cmd.Run(); err != nil {
		run.Err = err.Error() + ": " + truncate(stderr.String(), 512)
		return run
	}
	var env recallEnvelope
	dec := json.NewDecoder(strings.NewReader(stdout.String()))
	if err := dec.Decode(&env); err != nil && err != io.EOF {
		run.Err = "decode: " + err.Error()
		return run
	}
	for i, r := range env.Results {
		run.Hits = append(run.Hits, capturedHit{
			Rank:           i + 1,
			EntryID:        r.EntryID,
			Module:         extractModule(r.Body),
			Score:          r.Score,
			BaseActivation: r.BaseActivation,
			PPRScore:       r.PPRScore,
			Similarity:     r.Similarity,
			Importance:     r.Importance,
			WhySurfaced:    r.WhySurfaced,
			Body:           r.Body,
		})
	}
	return run
}

func extractModule(body string) string {
	m := modulePathRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimRight(m[1], ".")
}

func computeMetrics(q question, runs []retrievalRun, negThreshold float64) metrics {
	var m metrics
	if q.ShouldBeEmpty {
		triggered := true
		for _, r := range runs {
			if len(r.Hits) > 0 && r.Hits[0].Score >= negThreshold {
				triggered = false
				break
			}
		}
		m.NegativeTriggered = &triggered
		return m
	}
	if len(runs) == 0 {
		return m
	}
	expected := make(map[string]struct{}, len(q.ExpectedModules))
	for _, e := range q.ExpectedModules {
		expected[e] = struct{}{}
	}
	primary := runs[0]
	if len(expected) > 0 {
		if hitsExpectedAt(primary.Hits, expected, 1) {
			m.PAt1 = 1
		}
		if hitsExpectedAt(primary.Hits, expected, 3) {
			m.PAt3 = 1
		}
		m.MRR = reciprocalRank(primary.Hits, expected)
	}
	// Rephrasing agreement: fraction of rephrasings whose top-1 module
	// equals the primary top-1 module. Module-level, not chunk-level.
	if len(runs) > 1 {
		var primaryMod string
		if len(primary.Hits) > 0 {
			primaryMod = primary.Hits[0].Module
		}
		agree := 0
		total := 0
		for _, r := range runs[1:] {
			total++
			if len(r.Hits) > 0 && r.Hits[0].Module == primaryMod && primaryMod != "" {
				agree++
			}
		}
		if total > 0 {
			m.RephrasingAgreement = float64(agree) / float64(total)
		}
	}
	return m
}

func hitsExpectedAt(hits []capturedHit, expected map[string]struct{}, k int) bool {
	for i, h := range hits {
		if i >= k {
			break
		}
		if _, ok := expected[h.Module]; ok {
			return true
		}
	}
	return false
}

func reciprocalRank(hits []capturedHit, expected map[string]struct{}) float64 {
	for i, h := range hits {
		if _, ok := expected[h.Module]; ok {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func ratioF(n, d float64) float64 {
	if d == 0 {
		return 0
	}
	return n / d
}
