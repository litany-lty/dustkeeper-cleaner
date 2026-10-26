package domain

import "time"

// Budget accounting is split into three ledgers, because one struct was doing
// three jobs and each job has a different lifetime:
//
//	RunLedger      one per run: monotonic deadline, bytes actually read, pacing
//	ActionLedger   one per action class (quarantine / purge), shared by every
//	               scope and every rule in that run
//	Budget         a phase view over the two above, optionally with its own
//	               allowance so one phase cannot spend the whole run
//
// The split matters for correctness, not tidiness. A child view used to copy
// the parent's counters, so two scopes could each spend the same "remaining"
// bytes and the report understated real I/O. Now a child reads the shared
// ledger and every charge lands back in it.

// RunLedger is the single per-run ledger for time and read I/O.
type RunLedger struct {
	SoftTime   time.Duration
	MaxRead    int64
	Read       int64
	RatePerSec int64
}

// NewRunLedger builds the run ledger from configured limits.
func NewRunLedger(softSeconds int, maxRead, ratePerSec int64) *RunLedger {
	return &RunLedger{
		SoftTime:   time.Duration(softSeconds) * time.Second,
		MaxRead:    maxRead,
		RatePerSec: ratePerSec,
	}
}

// TimeUp reports whether the soft time slice is spent. It is a safe-boundary
// signal, not a kill deadline: a caller may finish the object it already
// changed, but must not start a new business action.
func (l *RunLedger) TimeUp(mono time.Duration) bool {
	if l == nil || l.SoftTime <= 0 {
		return false
	}
	return mono >= l.SoftTime
}

// ReadUp reports whether the read budget is spent.
func (l *RunLedger) ReadUp() bool {
	return l != nil && l.MaxRead > 0 && l.Read >= l.MaxRead
}

// AddRead accounts for bytes actually read. Reads are never refunded: the disk
// did the work even when the action was abandoned.
func (l *RunLedger) AddRead(n int64) {
	if l != nil {
		l.Read += n
	}
}

// RemainingRead returns the unspent read allowance.
func (l *RunLedger) RemainingRead() int64 {
	if l == nil || l.MaxRead <= 0 {
		return 0
	}
	if l.Read >= l.MaxRead {
		return 0
	}
	return l.MaxRead - l.Read
}

// Rate returns the pacing rate, or zero when unpaced.
func (l *RunLedger) Rate() int64 {
	if l == nil {
		return 0
	}
	return l.RatePerSec
}

// ActionLedger counts actions of one class for the whole run.
//
// A zero limit means that class is forbidden, exactly as section 4.3 requires.
// It never means unlimited: an ability to delete files must not appear because a
// field was left out of a configuration file.
type ActionLedger struct {
	MaxFiles int
	MaxBytes int64
	Files    int
	Bytes    int64
}

// NewActionLedger builds a shared action ledger.
func NewActionLedger(maxFiles int, maxBytes int64) *ActionLedger {
	return &ActionLedger{MaxFiles: maxFiles, MaxBytes: maxBytes}
}

// CanReserve reports whether one more action of this logical size fits.
func (l *ActionLedger) CanReserve(size int64) bool {
	if l == nil {
		return true
	}
	if l.MaxFiles <= 0 || l.MaxBytes <= 0 {
		return false
	}
	if l.Files+1 > l.MaxFiles {
		return false
	}
	if size > 0 && l.Bytes+size > l.MaxBytes {
		return false
	}
	return true
}

// Add records one committed action.
func (l *ActionLedger) Add(size int64) {
	if l == nil {
		return
	}
	l.Files++
	l.Bytes += size
}

// RemainingFiles returns how many more actions the run may still perform.
func (l *ActionLedger) RemainingFiles() int {
	if l == nil {
		return 0
	}
	if r := l.MaxFiles - l.Files; r > 0 {
		return r
	}
	return 0
}

// RemainingBytes returns how many more logical bytes the run may still move.
func (l *ActionLedger) RemainingBytes() int64 {
	if l == nil {
		return 0
	}
	if r := l.MaxBytes - l.Bytes; r > 0 {
		return r
	}
	return 0
}

// Budget is the view one phase holds.
type Budget struct {
	Run     *RunLedger
	Actions *ActionLedger

	// Capped says whether this phase has its own allowance. When it is false the
	// shared ledgers are the only limit, which is the right shape for a small
	// bounded step such as one item's post-move verification.
	Capped   bool
	MaxFiles int
	MaxBytes int64
	Files    int
	Bytes    int64
}

// NewBudget builds a self-contained budget. It is used by tests and by the CLI
// where one command is the whole run.
func NewBudget(softSeconds int, maxRead int64, maxFiles int, maxBytes int64, ratePerSec int64) *Budget {
	return NewPhaseBudget(NewRunLedger(softSeconds, maxRead, ratePerSec),
		NewActionLedger(maxFiles, maxBytes), maxFiles, maxBytes)
}

// NewPhaseBudget builds a phase view over shared ledgers.
func NewPhaseBudget(run *RunLedger, actions *ActionLedger, maxFiles int, maxBytes int64) *Budget {
	return &Budget{Run: run, Actions: actions, Capped: true, MaxFiles: maxFiles, MaxBytes: maxBytes}
}

// NewUncappedBudget builds a view that is limited only by the shared ledgers.
func NewUncappedBudget(run *RunLedger, actions *ActionLedger) *Budget {
	return &Budget{Run: run, Actions: actions}
}

// TimeUp reports whether the run's soft time slice is spent.
func (b *Budget) TimeUp(c Clock) bool {
	if b == nil || b.Run == nil {
		return false
	}
	return b.Run.TimeUp(c.Mono())
}

// ReadUp reports whether the run's read allowance is spent.
func (b *Budget) ReadUp() bool { return b != nil && b.Run.ReadUp() }

// AddRead charges bytes to the shared run ledger.
func (b *Budget) AddRead(n int64) {
	if b != nil {
		b.Run.AddRead(n)
	}
}

// RemainingRead returns the run's unspent read allowance.
func (b *Budget) RemainingRead() int64 { return b.Run.RemainingRead() }

// BytesRead returns the run's total read bytes.
func (b *Budget) BytesRead() int64 {
	if b == nil || b.Run == nil {
		return 0
	}
	return b.Run.Read
}

// Rate returns the pacing rate of the run.
func (b *Budget) Rate() int64 {
	if b == nil {
		return 0
	}
	return b.Run.Rate()
}

// CanReserve reports whether one more action of this size fits, both in this
// phase's allowance and in the run's shared ledger.
//
// It must be asked before the expensive part of the action (the content read),
// not after: discovering the shortfall after hashing wastes the read, and for a
// purge it means the object was re-verified for nothing.
func (b *Budget) CanReserve(size int64) bool {
	if b == nil {
		return true
	}
	if b.Capped {
		if b.MaxFiles <= 0 || b.MaxBytes <= 0 {
			return false
		}
		if b.Files+1 > b.MaxFiles {
			return false
		}
		if size > 0 && b.Bytes+size > b.MaxBytes {
			return false
		}
	}
	return b.Actions.CanReserve(size)
}

// AddAction records one committed action in both this phase and the run.
func (b *Budget) AddAction(size int64) {
	if b == nil {
		return
	}
	b.Files++
	b.Bytes += size
	b.Actions.Add(size)
}

// RemainingFiles reports how many more actions this phase may perform.
func (b *Budget) RemainingFiles() int {
	if b == nil {
		return 0
	}
	limit := b.Actions.RemainingFiles()
	if b.Capped {
		if r := b.MaxFiles - b.Files; r < limit {
			limit = r
		}
	}
	if limit < 0 {
		return 0
	}
	return limit
}

// RemainingBytes reports how many more logical bytes this phase may move.
func (b *Budget) RemainingBytes() int64 {
	if b == nil {
		return 0
	}
	limit := b.Actions.RemainingBytes()
	if b.Capped {
		if r := b.MaxBytes - b.Bytes; r < limit {
			limit = r
		}
	}
	if limit < 0 {
		return 0
	}
	return limit
}

// SplitFor narrows the phase allowance for a sub-step while keeping both shared
// ledgers attached. Reading in the child shows up in the parent, and the parent
// deadline still applies: a sub-step does not get a second time slice.
func (b *Budget) SplitFor(numerator, denominator int64) *Budget {
	if b == nil {
		return nil
	}
	if denominator <= 0 || numerator <= 0 || numerator >= denominator {
		return NewPhaseBudget(b.Run, b.Actions, b.RemainingFiles(), b.RemainingBytes())
	}
	shareFiles := int(int64(b.RemainingFiles()) * numerator / denominator)
	shareBytes := b.RemainingBytes() * numerator / denominator
	if shareFiles < 0 {
		shareFiles = 0
	}
	if shareBytes < 0 {
		shareBytes = 0
	}
	return NewPhaseBudget(b.Run, b.Actions, shareFiles, shareBytes)
}
