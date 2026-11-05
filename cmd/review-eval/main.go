// Command review-eval runs a bounded, suggestion-only evaluation. It never
// opens the Dustkeeper database and has no file-operation capability.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"sort"
	"strings"

	"dustkeeper/internal/config"
	"dustkeeper/internal/review"
	"dustkeeper/internal/strictjson"
)

const (
	maxEvaluationCases     = 6 // At most 12 model requests: JEV then MiniMax.
	maxMiniMaxOutputTokens = 4096
	maxSampleBytes         = 256 << 10
	// After two consecutive transport failures the path is treated as broken
	// and the run stops instead of spending the remaining quota on it.
	maxConsecutiveModelFailures = 2
)

//go:embed samples.synthetic.json
var embeddedSamples []byte

type labeledSample struct {
	ID        string       `json:"id"`
	Synthetic bool         `json:"synthetic"`
	Candidate review.Facts `json:"candidate"`
	Expected  string       `json:"expected"`
}

type dependencies struct {
	loadSettings func(string) (*review.Settings, error)
	newJudge     func(*review.Settings) (review.Judge, error)
	loadSamples  func(string) ([]byte, error)
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, productionDependencies()))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) int {
	fs := flag.NewFlagSet("review-eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "Dustkeeper config containing review settings")
	samplesPath := fs.String("samples", "", "optional synthetic sample JSON; built-in samples are the default")
	maxCases := fs.Int("max-cases", 0, "required cap (1..6; at most two model requests per case)")
	execute := fs.Bool("execute", false, "perform model calls; omitted means a zero-call dry-run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *configPath == "" || *maxCases < 1 || *maxCases > maxEvaluationCases {
		fmt.Fprintln(stderr, "--config and --max-cases 1..6 are required; no positional arguments are accepted")
		return 2
	}
	settings, err := deps.loadSettings(*configPath)
	if err != nil || settings == nil {
		fmt.Fprintln(stderr, "review settings are unavailable")
		return 2
	}
	raw, err := deps.loadSamples(*samplesPath)
	if err != nil {
		fmt.Fprintln(stderr, "synthetic samples are unavailable")
		return 2
	}
	samples, err := decodeSamples(raw)
	if err != nil {
		fmt.Fprintln(stderr, "synthetic samples are invalid")
		return 2
	}
	selected := min(*maxCases, len(samples))
	minConfidence, minAllowProbability := settings.Thresholds()
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{
		"type": "plan", "execute": *execute, "synthetic_only": true,
		"available": len(samples), "selected": selected, "max_cases": *maxCases,
		"max_model_calls":                    selected * 2,
		"max_minimax_output_tokens_per_call": maxMiniMaxOutputTokens,
		"max_minimax_output_tokens_total":    selected * maxMiniMaxOutputTokens,
		"jev_model":                          settings.JEVModel, "llm_model": settings.MiniMaxModel,
		// The gate and the per-call bound the run will apply. Without them a
		// "never allowed" result looks like a model opinion instead of a
		// threshold nobody can reach, and a lost case looks like a model error.
		"min_confidence": minConfidence, "min_allow_probability": minAllowProbability,
		"timeout_seconds":  settings.Timeout().Seconds(),
		"credentials_read": false, "credentials_required": *execute, "usage_supported": true, "jev_usage_supported": true, "jev_proxy_configured": settings.JEVProxyURL != "",
	}); err != nil {
		return 2
	}
	if !*execute {
		return 0
	}

	judge, err := deps.newJudge(settings)
	if err != nil {
		writeSummary(encoder, evaluationSummary{Selected: selected, StopReason: "model_unavailable"})
		fmt.Fprintln(stderr, "model judge is unavailable; evaluation stopped")
		return 2
	}
	summary := evaluationSummary{Selected: selected}
	consecutiveFailures := 0
	for i := 0; i < selected; i++ {
		sample := samples[i]
		decision, reviewErr := judge.Review(ctx, sample.Candidate)
		summary.Attempted++
		result := caseResult{LLMUsage: decision.LLMUsage, JEV: decision.JEV, Type: "case", Index: i + 1, ID: sample.ID, Expected: sample.Expected,
			Allow: decision.Verdict == "allow", Reason: decision.Reason, JEVModel: decision.JEVModel, LLMModel: decision.LLMModel,
			// The judge already records why a call failed; without this the
			// record says only "model_error" and the next person has to guess
			// between a timeout, a bad response and a refused key.
			Detail: decision.ModelError}
		if reviewErr != nil || decision.JEVModel == "" || (decision.Verdict == "allow" && decision.LLMModel == "") {
			result.Error = "model_error"
			_ = encoder.Encode(result)
			summary.Errors++
			consecutiveFailures++
			// One failed call is a measurement: the remaining samples still
			// say something about the reviewers. Two in a row means the path
			// itself is broken, and further calls would spend quota on the
			// same failure without answering anything.
			if consecutiveFailures >= maxConsecutiveModelFailures {
				break
			}
			continue
		}
		consecutiveFailures = 0
		result.Actual = "keep"
		switch decision.Verdict {
		case "allow":
			result.Actual = "allow"
		case "insufficient":
			result.Actual = "insufficient"
			summary.Abstentions++
		}
		result.Judgment = result.Actual
		result.Match = result.Actual == sample.Expected
		if !result.Match {
			summary.Mismatches++
		}
		summary.addModels(decision)
		if err := encoder.Encode(result); err != nil {
			return 2
		}
		if sample.Expected != "allow" && decision.Verdict == "allow" {
			summary.StopReason = "false_allow"
			writeSummary(encoder, summary)
			return 1
		}
	}
	// "model_unavailable" and "model_error" are different findings: the first
	// means nothing was measured and the configuration is at fault, the second
	// means some samples were measured and the rest were not.
	switch {
	case summary.Errors > 0 && summary.Errors == summary.Attempted:
		summary.StopReason = "model_unavailable"
	case summary.Errors > 0:
		summary.StopReason = "model_error"
	case summary.Abstentions > 0:
		summary.StopReason = "inconclusive"
	}
	if err := writeSummary(encoder, summary); err != nil {
		return 2
	}
	if summary.Errors != 0 {
		return 2
	}
	if summary.Mismatches != 0 || summary.Abstentions != 0 {
		return 1
	}
	return 0
}

type caseResult struct {
	LLMUsage *review.TokenUsage    `json:"llm_usage,omitempty"`
	JEV      *review.JEVDiagnostic `json:"jev,omitempty"`
	Type     string                `json:"type"`
	Index    int                   `json:"index"`
	ID       string                `json:"id"`
	Expected string                `json:"expected"`
	Actual   string                `json:"actual,omitempty"`
	Judgment string                `json:"judgment,omitempty"`
	Allow    bool                  `json:"allow"`
	Match    bool                  `json:"match"`
	Reason   string                `json:"reason,omitempty"`
	JEVModel string                `json:"jev_model,omitempty"`
	LLMModel string                `json:"llm_model,omitempty"`
	Error    string                `json:"error,omitempty"`
	Detail   string                `json:"error_detail,omitempty"`
}

type evaluationSummary struct {
	Type        string   `json:"type"`
	Selected    int      `json:"selected"`
	Attempted   int      `json:"attempted"`
	Mismatches  int      `json:"mismatches"`
	Abstentions int      `json:"abstentions"`
	Errors      int      `json:"errors,omitempty"`
	StopReason  string   `json:"stop_reason,omitempty"`
	JEVModels   []string `json:"jev_models,omitempty"`
	LLMModels   []string `json:"llm_models,omitempty"`
	jevSet      map[string]struct{}
	llmSet      map[string]struct{}
}

func (s *evaluationSummary) addModels(decision review.Decision) {
	if s.jevSet == nil {
		s.jevSet, s.llmSet = make(map[string]struct{}), make(map[string]struct{})
	}
	if decision.JEVModel != "" {
		s.jevSet[decision.JEVModel] = struct{}{}
	}
	if decision.LLMModel != "" {
		s.llmSet[decision.LLMModel] = struct{}{}
	}
}

func writeSummary(encoder *json.Encoder, summary evaluationSummary) error {
	summary.Type = "summary"
	for model := range summary.jevSet {
		summary.JEVModels = append(summary.JEVModels, model)
	}
	for model := range summary.llmSet {
		summary.LLMModels = append(summary.LLMModels, model)
	}
	sort.Strings(summary.JEVModels)
	sort.Strings(summary.LLMModels)
	return encoder.Encode(summary)
}

func decodeSamples(raw []byte) ([]labeledSample, error) {
	var samples []labeledSample
	opts := strictjson.Defaults()
	opts.MaxBytes, opts.MaxDepth = maxSampleBytes, 8
	if err := strictjson.Unmarshal(raw, &samples, opts); err != nil || len(samples) == 0 {
		return nil, errors.New("invalid sample document")
	}
	ids := make(map[string]struct{}, len(samples))
	for _, sample := range samples {
		candidate := sample.Candidate
		if sample.ID == "" || !sample.Synthetic ||
			(sample.Expected != "allow" && sample.Expected != "keep" && sample.Expected != "insufficient") ||
			candidate.ScopeID != "synthetic" || !strings.HasPrefix(candidate.ContentSHA256, "synthetic:") ||
			(candidate.Action != "delete" && candidate.Action != "quarantine") || candidate.RelativePath == "" ||
			candidate.Category == "" {
			return nil, errors.New("only labeled synthetic candidates are accepted")
		}
		if _, exists := ids[sample.ID]; exists {
			return nil, errors.New("duplicate sample id")
		}
		ids[sample.ID] = struct{}{}
	}
	return samples, nil
}

func productionDependencies() dependencies {
	return dependencies{
		loadSettings: func(path string) (*review.Settings, error) {
			cfg, defaults, err := config.Load(path)
			if err != nil || defaults || cfg.Review == nil {
				return nil, errors.New("configured review settings required")
			}
			return cfg.Review, nil
		},
		newJudge: newJudgeFromPrivateSettings,
		loadSamples: func(path string) ([]byte, error) {
			if path == "" {
				return embeddedSamples, nil
			}
			fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return nil, err
			}
			f := os.NewFile(uintptr(fd), path)
			defer f.Close()
			info, err := f.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() > maxSampleBytes {
				return nil, errors.New("sample input must be a bounded regular file")
			}
			data, err := io.ReadAll(io.LimitReader(f, maxSampleBytes+1))
			if err != nil || len(data) > maxSampleBytes {
				return nil, errors.New("sample input read failed or exceeded limit")
			}
			return data, nil
		},
	}
}

func newJudgeFromPrivateSettings(settings *review.Settings) (review.Judge, error) {
	if settings == nil || settings.CredentialsFile == "" {
		return nil, errors.New("private credentials file required")
	}
	raw, err := review.ReadPrivateFile(settings.CredentialsFile, 16<<10)
	if err != nil {
		return nil, errors.New("private credentials unavailable")
	}
	keys := struct {
		JEV     string `json:"typesafe_api_key"`
		MiniMax string `json:"minimax_api_key"`
	}{}
	if err := strictjson.Unmarshal(raw, &keys, strictjson.Defaults()); err != nil {
		return nil, errors.New("private credentials invalid")
	}
	judge, err := review.NewHTTPJudge(
		review.Endpoint{ProxyURL: settings.JEVProxyURL, TrustCAFile: settings.TrustCAFile, URL: settings.JEVURL, Model: settings.JEVModel, APIKey: keys.JEV},
		review.Endpoint{TrustCAFile: settings.TrustCAFile, URL: settings.MiniMaxURL, Model: settings.MiniMaxModel, APIKey: keys.MiniMax},
	)
	if err != nil {
		return nil, errors.New("model judge unavailable")
	}
	// The evaluation exists to measure the gate, so it has to run with the gate
	// the installation actually configured, not with the built-in default.
	if err := settings.ApplyTo(judge); err != nil {
		return nil, errors.New("review thresholds invalid")
	}
	return judge, nil
}
