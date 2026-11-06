package review

import (
	"dustkeeper/internal/errs"
	"dustkeeper/internal/strictjson"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
)

// Settings contains paths and model names only, never credentials. No enabled
// flag exists: omitting configuration is a refusal, not an offline fallback.
type Settings struct {
	JEVProxyURL     string `json:"jev_proxy_url,omitempty"`
	TrustCAFile     string `json:"trust_ca_file,omitempty"`
	CredentialsFile string `json:"credentials_file"`
	JEVURL          string `json:"jev_url"`
	JEVModel        string `json:"jev_model"`
	MiniMaxURL      string `json:"minimax_url"`
	MiniMaxModel    string `json:"minimax_model"`
	MaxCandidates   int    `json:"max_candidates"`
	// JEVMinConfidencePct and JEVMinAllowProbabilityPct calibrate when JEV may
	// authorise an action, in whole percent. Omitted means the value this build
	// was validated against (90/90). They are settable because the gate is
	// conjunctive: with a provider whose scale never reaches the default,
	// neither deletion nor isolation can ever be authorised, and "never
	// authorises anything" is a product that silently does nothing.
	//
	// Percent, not a fraction: this configuration format rejects every number
	// written with a fraction, on purpose (see strictjson.IntegerOnly).
	JEVMinConfidencePct       *int `json:"jev_min_confidence_pct,omitempty"`
	JEVMinAllowProbabilityPct *int `json:"jev_min_allow_probability_pct,omitempty"`
	// TimeoutSeconds bounds one provider call, in seconds. Omitted means 30.
	// It is settable because a slow provider turns a timeout into "the review
	// could not complete", which skips the file rather than waiting for it.
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
	// The batch allowances below bound one dispatch unit (one plan for one
	// rule). They are separate from MaxCandidates on purpose: raising the
	// candidate count never bought more time, and a batch that could not finish
	// its own plan reported the shortfall as a provider outage.
	//
	// BatchMaxCandidates omitted means 5.
	BatchMaxCandidates *int `json:"batch_max_candidates,omitempty"`
	// BatchSeconds omitted means max(180, 2*timeout + reserve).
	BatchSeconds *int `json:"batch_seconds,omitempty"`
	// BatchMaxAttempts bounds HTTP attempts per batch; omitted means 10.
	BatchMaxAttempts *int `json:"batch_max_attempts,omitempty"`
	// ReserveSeconds is kept free at the end of a batch; omitted means 10.
	ReserveSeconds *int `json:"reserve_seconds,omitempty"`
}

// Timeout reports the per-call bound these settings select.
func (s *Settings) Timeout() time.Duration {
	if s == nil || s.TimeoutSeconds == nil {
		return requestTimeout
	}
	return time.Duration(*s.TimeoutSeconds) * time.Second
}

// ApplyTo hands the calibrated gate and timeout to a judge built outside
// NewService (the evaluation command builds its own). Unset fields keep the
// defaults.
func (s *Settings) ApplyTo(j *HTTPJudge) error {
	if s == nil {
		return nil
	}
	if s.JEVMinConfidencePct != nil || s.JEVMinAllowProbabilityPct != nil {
		conf, allow := s.Thresholds()
		if err := j.SetThresholds(conf, allow); err != nil {
			return err
		}
	}
	if s.TimeoutSeconds != nil {
		if err := j.SetTimeout(s.Timeout()); err != nil {
			return err
		}
	}
	return nil
}

// Thresholds reports the gate these settings select, defaulting where unset.
// It is available without building a judge, so a zero-call plan can state the
// gate it is about to exercise.
func (s *Settings) Thresholds() (minConfidence, minAllowProbability float64) {
	conf, allow := jevMinConfidence, jevMinAllowProbability
	if s == nil {
		return conf, allow
	}
	if s.JEVMinConfidencePct != nil {
		conf = float64(*s.JEVMinConfidencePct) / 100
	}
	if s.JEVMinAllowProbabilityPct != nil {
		allow = float64(*s.JEVMinAllowProbabilityPct) / 100
	}
	return conf, allow
}

// Limits reports the batch allowances these settings select, with the same
// defaults NewService uses. Validation has already refused an inconsistent
// combination, so this cannot silently shrink a batch below one review path.
func (s *Settings) Limits() Limits {
	l := Limits{
		Candidates:   defaultBatchCandidates,
		Attempts:     defaultBatchAttempts,
		Reserve:      defaultBatchReserve,
		PerCandidate: 2 * requestTimeout,
	}
	if s == nil {
		l.Seconds = defaultBatchSeconds
		return l
	}
	if s.BatchMaxCandidates != nil {
		l.Candidates = *s.BatchMaxCandidates
	}
	if s.BatchMaxAttempts != nil {
		l.Attempts = *s.BatchMaxAttempts
	}
	if s.ReserveSeconds != nil {
		l.Reserve = time.Duration(*s.ReserveSeconds) * time.Second
	}
	l.PerCandidate = 2 * s.Timeout()
	l.Seconds = defaultBatchSeconds
	if s.BatchSeconds != nil {
		l.Seconds = *s.BatchSeconds
	}
	out, _ := l.consistent()
	return out
}

// consistent raises the batch window to fit one full review path plus the
// reserve, and reports whether the input was already consistent.
func (l Limits) consistent() (Limits, bool) {
	need := int((l.PerCandidate + l.Reserve + time.Second - 1) / time.Second)
	if l.Seconds >= need {
		return l, true
	}
	l.Seconds = need
	return l, false
}

func (s *Settings) Validate() error {
	if s == nil {
		return nil
	}
	if s.MaxCandidates < 1 || s.MaxCandidates > 50 {
		return errs.New(errs.CodeInvalidArgument, "review requires max_candidates 1..50")
	}
	for name, v := range map[string]*int{
		"jev_min_confidence_pct":        s.JEVMinConfidencePct,
		"jev_min_allow_probability_pct": s.JEVMinAllowProbabilityPct,
	} {
		if v == nil {
			continue
		}
		if *v < 1 || *v > 100 {
			return errs.Newf(errs.CodeInvalidArgument, "review.%s must be 1..100", name)
		}
	}
	for _, v := range []string{s.JEVURL, s.MiniMaxURL} {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !validURLPort(u) {
			return errs.New(errs.CodeInvalidArgument, "review endpoints must be HTTPS without credentials, query or fragment")
		}
	}
	if s.JEVProxyURL != "" {
		if _, err := parseProxy(s.JEVProxyURL); err != nil {
			return errs.New(errs.CodeInvalidArgument, "invalid review.jev_proxy_url")
		}
	} else {
		u, _ := url.Parse(s.JEVURL)
		if !isLoopbackHost(u.Hostname()) {
			return errs.New(errs.CodeInvalidArgument, "review.jev_proxy_url is required for external JEV; direct fallback is disabled")
		}
	}
	if !exactNonBlank(s.JEVModel) || !exactNonBlank(s.MiniMaxModel) {
		return errs.New(errs.CodeInvalidArgument, "review model names are required without boundary whitespace")
	}
	// The batch allowances must be able to contain one full review path plus the
	// reserve. Without this check a configuration could keep a 120-second batch
	// around a 60+60 second double call, and the second half would always be
	// abandoned after it had already been paid for.
	for name, v := range map[string]*int{
		"batch_max_candidates": s.BatchMaxCandidates,
		"batch_seconds":        s.BatchSeconds,
		"batch_max_attempts":   s.BatchMaxAttempts,
		"reserve_seconds":      s.ReserveSeconds,
	} {
		if v == nil {
			continue
		}
		if *v < 1 {
			return errs.Newf(errs.CodeInvalidArgument, "review.%s must be at least 1", name)
		}
	}
	if s.BatchMaxCandidates != nil && *s.BatchMaxCandidates > 50 {
		return errs.New(errs.CodeInvalidArgument, "review.batch_max_candidates must be at most 50")
	}
	if s.BatchMaxAttempts != nil && *s.BatchMaxAttempts > 50 {
		return errs.New(errs.CodeInvalidArgument, "review.batch_max_attempts must be at most 50")
	}
	if s.BatchSeconds != nil && *s.BatchSeconds > 3600 {
		return errs.New(errs.CodeInvalidArgument, "review.batch_seconds must be at most 3600")
	}
	if s.ReserveSeconds != nil && *s.ReserveSeconds > 120 {
		return errs.New(errs.CodeInvalidArgument, "review.reserve_seconds must be at most 120")
	}
	limits := s.Limits()
	need := int((2*s.Timeout() + limits.Reserve + time.Second - 1) / time.Second)
	if s.BatchSeconds != nil && *s.BatchSeconds < need {
		return errs.Newf(errs.CodeInvalidArgument,
			"review.batch_seconds (%d) cannot contain one dual review path plus reserve (%d)", *s.BatchSeconds, need)
	}
	return nil
}

func exactNonBlank(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// NewService builds the review service. Credentials are read here, so a command
// that never needs a model (status, restore, reconcile) never touches them.
func (s *Settings) NewService() (*Service, error) {
	if s == nil {
		return nil, refused("configure dual review before any quarantine or deletion")
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	keys := struct {
		JEV     string `json:"typesafe_api_key"`
		MiniMax string `json:"minimax_api_key"`
	}{os.Getenv("TYPESAFE_API_KEY"), os.Getenv("MINIMAX_API_KEY")}
	if s.CredentialsFile != "" {
		b, err := ReadPrivateFile(s.CredentialsFile, 16<<10)
		if err != nil {
			return nil, refused("private credentials file unavailable")
		}
		if err := decodeCredentialKeys(b, &keys); err != nil {
			return nil, refused("invalid private credentials file")
		}
	}
	judge, err := NewHTTPJudge(Endpoint{ProxyURL: s.JEVProxyURL, TrustCAFile: s.TrustCAFile, URL: s.JEVURL, Model: s.JEVModel, APIKey: keys.JEV}, Endpoint{TrustCAFile: s.TrustCAFile, URL: s.MiniMaxURL, Model: s.MiniMaxModel, APIKey: keys.MiniMax})
	if err != nil {
		return nil, errs.New(errs.CodeModelUnavailable, "dual review credentials or endpoints unavailable")
	}
	if err := s.ApplyTo(judge); err != nil {
		return nil, errs.New(errs.CodeInvalidArgument, "review thresholds are invalid")
	}
	return &Service{Judge: judge, MaxCandidates: s.MaxCandidates, Defaults: s.Limits()}, nil
}

func decodeCredentialKeys(b []byte, dst any) error {
	if err := strictjson.Unmarshal(b, dst, strictjson.Defaults()); err != nil {
		return err
	}
	presence, err := strictjson.Inspect(b)
	if err != nil {
		return err
	}
	fields := []strictjson.Field{
		{Path: "typesafe_api_key", Only: []strictjson.Shape{strictjson.ShapeScalar}},
		{Path: "minimax_api_key", Only: []strictjson.Shape{strictjson.ShapeScalar}},
	}
	if err := strictjson.Require(presence, fields); err != nil {
		return err
	}
	return strictjson.Allow(presence, fields)
}
