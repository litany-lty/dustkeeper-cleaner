package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Service reviews candidates with two independent models.
//
// It holds no file paths to read and no credentials of its own: the judge is
// injected, so a test can supply a deterministic one and a production caller
// supplies one built from the configured endpoints.
type Service struct {
	Judge Judge
	Now   func() time.Time
	// MaxCandidates bounds how many candidates one command reviews. It is a
	// spend limit, not a safety limit: a candidate never reviewed is never
	// acted on either.
	MaxCandidates int
	checked       int
	started       time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Review asks the reviewers about one candidate and returns the normalised
// decision plus the approval it grants, if any.
//
// A model failure is not an error of this function: it comes back as a
// Decision whose ModelError is set and whose verdict is keep. The caller must
// be able to tell "nobody could be asked" from "the answer was no", and an
// error return would collapse both into "the batch stopped".
func (s *Service) Review(ctx context.Context, r Request) (Decision, Approval, error) {
	if s == nil || s.Judge == nil {
		return Decision{}, nil, errs.New(errs.CodeReviewRequired, "dual review is not configured")
	}
	if !ActionSupported(r.Action) {
		return Decision{}, nil, errs.New(errs.CodeInvalidArgument, "unsupported review action")
	}
	if r.ItemID == "" || r.ScopeID == "" || !utf8.ValidString(r.RelativePath) || r.RelativePath == "" ||
		strings.ContainsRune(r.RelativePath, '\uFFFD') || r.SizeBytes < 0 || r.Category == "" {
		return Decision{}, nil, errs.New(errs.CodeInvalidArgument, "incomplete candidate identity or category")
	}
	if s.started.IsZero() {
		s.started = time.Now()
	}
	if time.Since(s.started) > 120*time.Second {
		return Decision{ModelError: "review time budget exhausted", Verdict: domain.VerdictKeep}, nil, nil
	}
	max := s.MaxCandidates
	if max <= 0 {
		max = 10
	}
	if s.checked >= max {
		return Decision{ModelError: "review candidate budget exhausted", Verdict: domain.VerdictKeep}, nil, nil
	}
	s.checked++

	facts := Facts{
		Action:        r.Action,
		FactsSource:   "measured_by_program",
		ScopeID:       r.ScopeID,
		RelativePath:  r.RelativePath,
		Category:      r.Category,
		SizeBytes:     r.SizeBytes,
		AgeDays:       r.AgeDays,
		Checks:        r.Checks,
		Observation:   r.Observation,
		Policy:        r.Policy,
		ContentSHA256: r.VerifiedContentSHA256,
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second-time.Since(s.started))
	defer cancel()
	decision, err := s.Judge.Review(ctx, facts)
	if err != nil {
		// The judge returns both a decision and an error for transport-level
		// failures; the decision already says "keep" and carries the reason.
		if decision.ModelError == "" {
			decision.ModelError = err.Error()
		}
		decision.Verdict = domain.VerdictKeep
		decision.LLMConsulted = false
		return decision, nil, nil
	}
	if decision.Verdict == "" {
		decision.Verdict = domain.VerdictKeep
		decision.ModelError = "reviewer returned no verdict"
		return decision, nil, nil
	}

	permit := s.permitFor(r, decision)
	return decision, permit, nil
}

// ActionSupported reports whether a review may be asked about this action.
//
// "delete" and "purge" are the same question asked of two different objects: a
// file at its original path, and an object that has already spent its
// observation period in the vault. Both are irreversible, and both require the
// reviewers to say so explicitly.
func ActionSupported(action string) bool {
	switch action {
	case domain.DispositionDelete, domain.ActionPurge, domain.DispositionQuarantine:
		return true
	}
	return false
}

// permanent reports whether the action destroys the bytes.
func permanent(action string) bool {
	return action == domain.DispositionDelete || action == domain.ActionPurge
}

// permitFor builds the approval a verdict grants.
//
// A permanent-deletion question answered "insufficient" is exactly the case the
// observation area exists for, so that answer authorises reversible isolation
// and nothing more. A quarantine question answered "insufficient" authorises
// nothing: the reviewers were asked whether moving is safe, and did not say so.
func (s *Service) permitFor(r Request, d Decision) Approval {
	var allowed []string
	switch d.Verdict {
	case domain.VerdictAllow:
		allowed = []string{r.Action}
	case domain.VerdictInsufficient:
		// "Not enough for permanent deletion" of a file still in place is the
		// exact case the observation area exists for. Asked about an object
		// already in that area, the same answer means "keep observing": there is
		// nothing reversible left to do.
		if permanent(r.Action) && r.Action == domain.DispositionDelete {
			allowed = []string{domain.DispositionQuarantine}
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	return &permit{
		service:    s,
		itemID:     r.ItemID,
		requested:  r.Action,
		allowed:    allowed,
		content:    r.VerifiedContentSHA256,
		decision:   d,
		created:    s.now().UnixNano(),
		expires:    s.now().Add(5 * time.Minute).UnixNano(),
		factsBytes: mustJSON(factsOf(r)),
	}
}

func factsOf(r Request) Facts {
	return Facts{
		Action: r.Action, FactsSource: "measured_by_program", ScopeID: r.ScopeID,
		RelativePath: r.RelativePath, Category: r.Category, SizeBytes: r.SizeBytes,
		AgeDays: r.AgeDays, Checks: r.Checks, Observation: r.Observation,
		Policy: r.Policy, ContentSHA256: r.VerifiedContentSHA256,
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// permit is the approval that binds a verdict to one item and one action.
//
// It deliberately does not hold model credentials, absolute paths or file
// content: an approval that leaks those into an audit record is a second copy
// of the thing it was supposed to protect.
type permit struct {
	service    *Service
	itemID     string
	requested  string
	allowed    []string
	content    string
	verified   string
	decision   Decision
	factsBytes []byte
	created    int64
	expires    int64
}

// Disposition is the product-level result this approval permits. "purge" is
// reported as a deletion: it is one of the three outcomes, not a fourth one.
func (p *permit) Disposition() string {
	if len(p.allowed) == 0 {
		return domain.DispositionKeep
	}
	if permanent(p.allowed[0]) {
		return domain.DispositionDelete
	}
	return p.allowed[0]
}

// Verify re-confirms the approval right before an action. When the reviewed
// candidate carried a content digest, the same digest must still be the one
// being acted on, so a file swapped during the review cannot inherit the
// approval.
func (p *permit) Verify(contentHash string) error {
	if p.content != "" && contentHash != "" && contentHash != p.content {
		return errs.New(errs.CodeReviewRequired, "the reviewed content is not the content about to be acted on")
	}
	// The hash the action was actually verified against is kept so the audit
	// record names those bytes. A review that measured no content leaves this
	// empty on purpose: an empty digest is honest, an invented one is not.
	if contentHash != "" {
		p.verified = contentHash
	}
	return p.CheckIntent(p.itemID, p.currentAction(), p.service.now().UnixNano())
}

// currentAction is the action the approval is being exercised for. The executor
// asks for the specific action, so Verify uses the first allowed one.
func (p *permit) currentAction() string {
	if len(p.allowed) == 0 {
		return ""
	}
	return p.allowed[0]
}

// CheckIntent is called inside the intent transaction. It performs no I/O and
// no model call: it only re-reads the facts the approval already contains.
func (p *permit) CheckIntent(itemID, action string, nowNS int64) error {
	if itemID != p.itemID {
		return errs.New(errs.CodeReviewRequired, "review does not bind this item")
	}
	allowed := false
	for _, a := range p.allowed {
		if a == action {
			allowed = true
			break
		}
	}
	if !allowed {
		return errs.Newf(errs.CodeReviewRequired,
			"review was asked about %q and does not authorise %q", p.requested, action)
	}
	if nowNS < p.created || nowNS >= p.expires {
		return errs.New(errs.CodeReviewRequired, "review expired")
	}
	if time.Since(p.service.started) > 120*time.Second {
		return errs.New(errs.CodeReviewRequired, "review time budget exhausted")
	}
	return nil
}

// AuditJSON is what an intent stores. It records what was asked, what was
// answered and which action was exercised, so "allowed" can never be read as a
// broader permission than it was.
func (p *permit) AuditJSON() string {
	b, _ := json.Marshal(struct {
		ItemID    string   `json:"item_id"`
		Requested string   `json:"requested_action"`
		Allowed   []string `json:"allowed_actions"`
		SHA256    string   `json:"sha256,omitempty"`
		Expires   int64    `json:"expires_at_ns"`
		Decision  Decision `json:"decision"`
	}{p.itemID, p.requested, p.allowed, p.verifiedHash(), p.expires, p.decision})
	return string(b)
}

// verifiedHash is the digest the action was checked against, falling back to
// the one the review itself measured.
func (p *permit) verifiedHash() string {
	if p.verified != "" {
		return p.verified
	}
	return p.content
}

// DigestOf is a helper for callers that want to log a stable identity of the
// facts a decision was made from, without storing them a second time.
func DigestOf(b []byte) string {
	d := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(d[:])
}
