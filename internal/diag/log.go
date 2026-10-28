// Package diag writes the structured operational log.
//
// Why it exists: every claim in the compliance table was, until now, supported by
// stdout and a database. Neither survives the process. When a scheduled run stops at
// three in the morning, the person looking at it the next day needs to know which
// run it was, which scope stopped it, what control level it reached, and what to try
// next — without reconstructing that from a JSON envelope nobody kept.
//
// Three rules shape this package:
//
//   - Bounded. The log rotates by size and keeps a fixed number of files, so a
//     machine left alone for a year cannot fill a disk that the program is supposed
//     to be freeing.
//   - No payload. Records carry identifiers, phases, codes and counts. File contents,
//     manifest bodies, model prompts and API keys never reach this package; a log
//     that leaks a credential is worse than no log at all.
//   - Not a monitoring system. One JSON object per line, standard library only, no
//     shipping, no sampling, no background goroutine.
package diag

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Bounds. They are constants rather than configuration on purpose: a bound that an
// operator can raise to "unlimited" is not a bound, and these are small enough that
// losing history is never a surprise.
const (
	// MaxFileBytes is the size at which the current file is rotated.
	MaxFileBytes = 2 << 20
	// KeepFiles is how many rotated files are kept after rotation. The live file plus
	// these backup files is the whole on-disk footprint.
	KeepFiles = 3
	// FileName is the live log file inside the log directory.
	FileName = "dustkeeper.jsonl"
	// MaxFieldBytes caps one string value in a record. It is the difference between
	// "bounded" and "bounded in theory": a single error string carrying a whole file
	// must not be able to fill the log, and nothing in a diagnostic record needs 4 KiB.
	MaxFieldBytes = 4 << 10
	// truncated marks a value that hit the cap, so a reader is not misled by a
	// plausible-looking prefix.
	truncated = "...[truncated]"
)

// Logger is a bounded JSONL logger. A nil *Logger is valid and drops every record,
// so callers do not need to branch on whether logging was enabled.
type Logger struct {
	inner *slog.Logger
	rot   *rotating
}

// Open prepares the log in dir, creating the directory if needed.
//
// The file is opened under the private-file contract rather than with a plain
// O_CREATE|O_APPEND: the log names every scope and every decision, so a
// pre-created symlink, a FIFO, a hard link or a group-readable file is refused
// instead of written through (see openPrivateLog). Refusing is the only outcome
// that keeps the target unchanged.
//
// A failure to open the log is returned rather than swallowed: a run that believes
// it is recording and is not would be exactly the kind of silent gap this package is
// meant to close.
func Open(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("diag: create log directory %s: %w", dir, err)
	}
	rot, err := newRotating(filepath.Join(dir, FileName), MaxFileBytes, KeepFiles)
	if err != nil {
		return nil, err
	}
	h := slog.NewJSONHandler(rot, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		// The default replaces the message key with "msg"; keeping the default keeps
		// the format ordinary, which matters for anything that already reads JSONL.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Times are UTC seconds-resolution: a log compared across machines with
			// different zones is worse than no timestamp.
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().UTC().Format(time.RFC3339Nano))
			}
			return a
		},
	})
	return &Logger{inner: slog.New(h), rot: rot}, nil
}

// Log records one event. Pairs are key/value, as slog expects.
//
// String values are capped at MaxFieldBytes. A record is a pointer to what happened,
// not a copy of the data it happened to.
func (l *Logger) Log(msg string, kv ...any) {
	if l == nil || l.inner == nil {
		return
	}
	l.inner.Info(capString(msg), sanitize(kv)...)
}

// sanitize caps every string value, keeping the key/value shape slog expects.
func sanitize(kv []any) []any {
	out := make([]any, len(kv))
	copy(out, kv)
	for i, v := range out {
		// Keys are at even indexes and are never truncated: a shortened key would
		// change the meaning of the record.
		if i%2 == 1 {
			if s, ok := v.(string); ok {
				out[i] = capString(s)
			}
		}
	}
	return out
}

func capString(s string) string {
	if len(s) <= MaxFieldBytes {
		return s
	}
	return s[:MaxFieldBytes] + truncated
}

// Close flushes and releases the file.
func (l *Logger) Close() error {
	if l == nil || l.rot == nil {
		return nil
	}
	return l.rot.Close()
}

// Path returns the live log file path, for a diagnostic message.
func (l *Logger) Path() string {
	if l == nil || l.rot == nil {
		return ""
	}
	return l.rot.path
}

// rotating is an io.Writer that keeps the file under max bytes by shifting
// name -> name.1 -> name.2 ... and dropping the oldest beyond keep.
type rotating struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int

	file *os.File
	size int64
}

func newRotating(path string, max int64, keep int) (*rotating, error) {
	r := &rotating{path: path, max: max, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotating) open() error {
	f, err := openPrivateLog(r.path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("diag: stat %s: %w", r.path, err)
	}
	r.file = f
	r.size = info.Size()
	return nil
}

// Write appends one record, rotating first if the record would cross the bound.
//
// A single record larger than max still gets written: dropping it would lose the very
// event that explains a failure. The next write rotates, so the file cannot keep
// growing that way.
func (r *rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts the files and starts a new one.
func (r *rotating) rotate() error {
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("diag: close %s: %w", r.path, err)
	}
	r.file = nil

	// Oldest first: name.(keep-1) is dropped, then each file moves one step.
	oldest := fmt.Sprintf("%s.%d", r.path, r.keep)
	_ = os.Remove(oldest)
	for i := r.keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", r.path, i)
		to := fmt.Sprintf("%s.%d", r.path, i+1)
		// Lstat, not Stat: a rotated name that has become a link is moved as a
		// link, never resolved to the file it points at.
		if _, err := os.Lstat(from); err == nil {
			if err := os.Rename(from, to); err != nil {
				return fmt.Errorf("diag: rotate %s: %w", from, err)
			}
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return fmt.Errorf("diag: rotate %s: %w", r.path, err)
	}
	return r.open()
}

// Close releases the file.
func (r *rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
