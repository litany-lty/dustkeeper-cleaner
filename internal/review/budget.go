package review

import "time"

// Failure kinds.
//
// The distinction the product needs is not "did an error happen" but "did
// anything reach the provider". A batch that ran out of its slice before the
// first request never cost anything and never had a chance to answer; a request
// that was sent and timed out may already have been billed. Reporting both as
// MODEL_UNAVAILABLE is what made the operator believe a vendor outage had
// happened while the run had simply spent its own budget.
const (
	// FailureRunBudget — the whole task's review allowance is gone. No further
	// scope may dispatch a model request in this run.
	FailureRunBudget = "run_budget"
	// FailureBatchBudget — this batch may not start another candidate. The next
	// batch (a later rule or run) may, if the task still has room.
	FailureBatchBudget = "batch_budget"
	// FailureTimeout — a request was sent and no complete answer arrived in time.
	FailureTimeout = "timeout"
	// FailureAuth — the provider rejected the request (credentials or access).
	FailureAuth = "auth"
	// FailureProvider — the provider answered with an error status or an
	// unusable envelope.
	FailureProvider = "provider"
	// FailureTransport — the connection failed, was reset, or the request could
	// not be created.
	FailureTransport = "transport"
	// FailureResponse — bytes arrived but could not be turned into a decision.
	FailureResponse = "response"
)

// Cost states. They describe what is known about this request, never what was
// billed: without provider evidence an amount stays unknown.
const (
	CostNotSent       = "not_sent"
	CostUnknown       = "unknown"
	CostUsageRecorded = "usage_recorded"
)

// Limits are the bounds one batch of reviews may spend.
//
// A batch is the unit the executor dispatches: one plan for one rule. The task
// budget is not duplicated here — RunRemaining reports what the run ledger has
// left on the same monotonic clock, so a batch can never reset it by starting.
type Limits struct {
	// Candidates bounds distinct candidates reviewed in this batch.
	Candidates int
	// Attempts bounds HTTP attempts in this batch, not candidates: one candidate
	// may need two calls, and an extra action for the same file is a new call.
	Attempts int
	// Seconds bounds the batch's own wall time from BeginBatch.
	Seconds int
	// Reserve is kept free at the end for recording, reconciling and closing
	// logs. It is never available to a model request.
	Reserve time.Duration
	// PerCandidate is the worst-case review path for one candidate (both
	// providers, each allowed its full timeout). A candidate that cannot fit is
	// not started rather than started and abandoned.
	PerCandidate time.Duration
	// RunRemaining reports the task's remaining time. nil means the caller has
	// no task-level clock (a debug or single-candidate command).
	RunRemaining func() time.Duration
}

// Defaults this build starts from. They are ceilings, not promises: five
// candidates per batch says nothing about how many will fit in the time.
const (
	defaultBatchCandidates = 5
	defaultBatchAttempts   = 10
	defaultBatchSeconds    = 180
	defaultBatchReserve    = 10 * time.Second
)

// batchState is the live accounting of one batch.
type batchState struct {
	active  bool
	limits  Limits
	started time.Time
	// startedMono is the monotonic reading of the same instant. Elapsed time is
	// always measured against it: the wall clock can jump, and a batch that
	// looks eight months old because the machine's clock moved would report
	// "budget exhausted" for every candidate.
	startedMono time.Time
	candidates  int
	attempts    int
}

// BeginBatch opens a new batch with these limits. Counters restart here; the
// task-level candidate and attempt counts do not.
func (s *Service) BeginBatch(l Limits) {
	if s == nil {
		return
	}
	s.batch = batchState{active: true, limits: s.resolveLimits(l), started: s.now(), startedMono: time.Now()}
}

// EndBatch closes the current batch. The next Review without a BeginBatch opens
// an implicit one with the service defaults.
func (s *Service) EndBatch() {
	if s != nil {
		s.batch.active = false
	}
}

// Batch reports how much of the current batch has been spent.
func (s *Service) Batch() (candidates, attempts int, open bool) {
	if s == nil {
		return 0, 0, false
	}
	return s.batch.candidates, s.batch.attempts, s.batch.active
}

// CheckStart reports whether one more candidate may be reviewed, and if not,
// which allowance stopped it. It performs no I/O and makes no request.
func (s *Service) CheckStart() string {
	if s == nil {
		return FailureRunBudget
	}
	if s.batchOpen() {
		if s.runRemaining() < s.batch.limits.Reserve+s.batch.limits.PerCandidate {
			return FailureRunBudget
		}
		if time.Since(s.batch.startedMono)+s.batch.limits.PerCandidate+s.batch.limits.Reserve >
			time.Duration(s.batch.limits.Seconds)*time.Second {
			return FailureBatchBudget
		}
	}
	if max := s.maxCandidates(); max > 0 && s.checked >= max {
		return FailureRunBudget
	}
	if s.batchOpen() && s.batch.candidates >= s.batch.limits.Candidates {
		return FailureBatchBudget
	}
	if s.attempts >= s.attemptLimit() {
		return FailureRunBudget
	}
	if s.batchOpen() && s.batch.attempts >= s.batch.limits.Attempts {
		return FailureBatchBudget
	}
	return ""
}

// batchOpen opens an implicit batch the first time a caller that never calls
// BeginBatch (debug, a single review) asks a question.
func (s *Service) batchOpen() bool {
	if !s.batch.active {
		s.batch = batchState{active: true, limits: s.resolveLimits(Limits{}), started: s.now(), startedMono: time.Now()}
	}
	return true
}

func (s *Service) resolveLimits(l Limits) Limits {
	d := s.Defaults
	if l.Candidates <= 0 {
		l.Candidates = d.Candidates
	}
	if l.Candidates <= 0 {
		l.Candidates = defaultBatchCandidates
	}
	if l.Attempts <= 0 {
		l.Attempts = d.Attempts
	}
	if l.Attempts <= 0 {
		l.Attempts = defaultBatchAttempts
	}
	if l.Seconds <= 0 {
		l.Seconds = d.Seconds
	}
	if l.PerCandidate <= 0 {
		l.PerCandidate = d.PerCandidate
	}
	if l.PerCandidate <= 0 {
		l.PerCandidate = 2 * requestTimeout
	}
	if l.Reserve <= 0 {
		l.Reserve = d.Reserve
	}
	if l.Reserve <= 0 {
		l.Reserve = defaultBatchReserve
	}
	if l.Seconds <= 0 {
		l.Seconds = defaultBatchSeconds
	}
	if min := int((l.PerCandidate + l.Reserve + time.Second - 1) / time.Second); l.Seconds < min {
		// A batch that cannot fit one full review path plus its reserve would
		// start candidates it must abandon. The larger value wins here and the
		// settings validation refuses an inconsistent configuration earlier.
		l.Seconds = min
	}
	if l.RunRemaining == nil {
		l.RunRemaining = d.RunRemaining
	}
	return l
}

func (s *Service) runRemaining() time.Duration {
	if s.batch.limits.RunRemaining == nil {
		return time.Duration(1<<62 - 1)
	}
	return s.batch.limits.RunRemaining()
}

// remaining is the time one request may use: the smaller of what the batch has
// left after its reserve and what the task has left. It is never negative, so a
// caller that somehow asks anyway gets an immediate deadline instead of a
// request that overruns the batch.
func (s *Service) remaining() time.Duration {
	left := s.runRemaining()
	if s.batch.active {
		batchLeft := time.Duration(s.batch.limits.Seconds)*time.Second -
			time.Since(s.batch.startedMono) - s.batch.limits.Reserve
		if batchLeft < left {
			left = batchLeft
		}
	}
	if left < 0 {
		return time.Millisecond
	}
	return left
}

func (s *Service) maxCandidates() int {
	if s.MaxCandidates > 0 {
		return s.MaxCandidates
	}
	return 10
}

// attemptLimit is the task-wide ceiling on HTTP attempts. It is derived from the
// candidate ceiling because each candidate can need two calls; a caller that
// configured a smaller attempt budget gets it.
func (s *Service) attemptLimit() int {
	if s.MaxAttempts > 0 {
		return s.MaxAttempts
	}
	return 2 * s.maxCandidates()
}

// Expired reports whether an approval granted earlier in this command may still
// be exercised. A batch that is over, or a task whose allowance is gone, may not
// take a new file action even on an answer it already received: the retention
// window and the deadline it was checked against belong to that batch.
func (s *Service) Expired() bool {
	if s == nil {
		return false
	}
	if s.batch.active {
		if s.runRemaining() < s.batch.limits.Reserve {
			return true
		}
		if time.Since(s.batch.startedMono) > time.Duration(s.batch.limits.Seconds)*time.Second {
			return true
		}
	}
	return false
}
