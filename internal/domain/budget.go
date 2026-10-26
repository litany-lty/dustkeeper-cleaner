package domain

import "time"

// Budget bounds one run. File counts and logical bytes limit actions; read
// bytes limit I/O. They are counted separately because reading the same file
// twice really does consume the disk twice.
type Budget struct {
	SoftTime   time.Duration
	MaxRead    int64
	Read       int64
	MaxFiles   int
	Files      int
	MaxBytes   int64
	Bytes      int64
	RatePerSec int64
}

// NewBudget builds a budget from configured limits.
func NewBudget(softSeconds int, maxRead int64, maxFiles int, maxBytes int64, ratePerSec int64) *Budget {
	return &Budget{
		SoftTime:   time.Duration(softSeconds) * time.Second,
		MaxRead:    maxRead,
		MaxFiles:   maxFiles,
		MaxBytes:   maxBytes,
		RatePerSec: ratePerSec,
	}
}

// TimeUp reports whether the soft time slice is spent. This is a safe-boundary
// signal, not a kill deadline.
func (b *Budget) TimeUp(c Clock) bool {
	if b == nil || b.SoftTime <= 0 {
		return false
	}
	return c.Mono() >= b.SoftTime
}

// ReadUp reports whether the read budget is spent.
func (b *Budget) ReadUp() bool {
	return b != nil && b.MaxRead > 0 && b.Read >= b.MaxRead
}

// AddRead accounts for bytes actually read.
func (b *Budget) AddRead(n int64) {
	if b != nil {
		b.Read += n
	}
}

// CanReserve reports whether one more action of this size fits. A zero limit
// means the action class is forbidden, never unlimited.
func (b *Budget) CanReserve(size int64) bool {
	if b == nil {
		return true
	}
	if b.MaxFiles == 0 || b.MaxBytes == 0 {
		return false
	}
	if b.Files+1 > b.MaxFiles {
		return false
	}
	if b.Bytes+size > b.MaxBytes {
		return false
	}
	return true
}

// AddAction records one committed action.
func (b *Budget) AddAction(size int64) {
	if b == nil {
		return
	}
	b.Files++
	b.Bytes += size
}

// RemainingRead returns the unspent read allowance.
func (b *Budget) RemainingRead() int64 {
	if b == nil || b.MaxRead <= 0 {
		return 0
	}
	if b.Read >= b.MaxRead {
		return 0
	}
	return b.MaxRead - b.Read
}

// SplitFor divides a budget for a phase that may use at most a share of it.
// Purging may take half of what is left so that scanning and quarantine still
// make progress in the same run.
func (b *Budget) SplitFor(numerator, denominator int64) *Budget {
	if b == nil {
		return nil
	}
	share := func(v int64) int64 { return v * numerator / denominator }
	out := &Budget{
		SoftTime:   b.SoftTime / 2,
		MaxRead:    share(b.RemainingRead()),
		MaxFiles:   int(share(int64(b.MaxFiles - b.Files))),
		MaxBytes:   share(b.MaxBytes - b.Bytes),
		RatePerSec: b.RatePerSec,
	}
	if out.MaxFiles < 0 {
		out.MaxFiles = 0
	}
	if out.MaxBytes < 0 {
		out.MaxBytes = 0
	}
	return out
}
