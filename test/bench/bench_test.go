// Package bench measures the real binary against a large tree.
//
// It is deliberately a black-box benchmark: it builds cmd/dustkeeper, runs it as
// a subprocess and reads the peak resident set size and CPU time the kernel
// recorded for that process. Measuring internals instead would miss exactly what
// the requirement is about — that a scan of a very large directory does not grow
// with the number of files.
//
// The benchmark creates real files, so it does not run by default:
//
//	DUSTKEEPER_BENCH=1 go test ./test/bench -run TestLargeTree -v
//	DUSTKEEPER_BENCH=1 DUSTKEEPER_BENCH_OUT=docs/evidence/performance-100k.md \
//	    go test ./test/bench -run 'TestSmallTree|TestLargeTree' -v
//
// Three questions, recorded separately because they are different claims:
//
//  1. does a scan finish the whole tree, and what did it cost?
//  2. does one run under the default budget behave, and does it say whether it
//     finished or ran out of time?
//  3. after a budget stop, does resuming make progress without re-reading?
//
// What the evidence does not claim: file-content bytes read (the scan reads no
// content, so it is reported as null rather than as a zero measured value),
// physical disk reads (Rusage inblock is a block-layer hint, not a count of
// physical reads), and WAL bytes (the store fixes journal_mode=DELETE, so there is
// no WAL to measure).
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// defaults describe the acceptance target from section 15: 100 000 files under a
// resident set that does not scale with the count.
const (
	defaultFiles  = 100000
	defaultFanout = 1000 // leaf directories
	defaultMaxRSS = 256  // MiB
	smallFiles    = 2000
	smallFanout   = 40
)

type benchEnv struct {
	t      *testing.T
	bin    string
	state  string
	config string
	files  int
}

func newBenchEnv(t *testing.T) *benchEnv {
	t.Helper()
	if os.Getenv("DUSTKEEPER_BENCH") != "1" {
		t.Skip("set DUSTKEEPER_BENCH=1 to run the tree benchmarks")
	}
	if testing.Short() {
		t.Skip("skipping the tree benchmarks in -short mode")
	}

	files := defaultFiles
	if v := os.Getenv("DUSTKEEPER_BENCH_FILES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("DUSTKEEPER_BENCH_FILES=%q is not a positive integer", v)
		}
		files = n
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "dustkeeper")
	build := exec.Command("go", "build", "-o", bin, "./cmd/dustkeeper")
	build.Dir = "../.."
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build the binary: %v", err)
	}

	return &benchEnv{
		t:      t,
		bin:    bin,
		state:  filepath.Join(tmp, "state"),
		config: filepath.Join(tmp, "config"),
		files:  files,
	}
}

func (b *benchEnv) env() []string {
	return append(os.Environ(),
		"DUSTKEEPER_STATE_DIR="+b.state,
		"DUSTKEEPER_CONFIG_DIR="+b.config,
	)
}

// measured is what one subprocess did: its output, its conclusion, and the
// resources the kernel attributes to it.
type measured struct {
	Output  string
	Err     error
	Wall    time.Duration
	User    time.Duration
	Sys     time.Duration
	MaxRSS  int64
	DiskIn  int64 // Rusage inblock: filesystem input operations, not bytes
	ExitOK  bool
	JSONEnv map[string]any
}

func (b *benchEnv) run(args ...string) measured {
	b.t.Helper()
	cmd := exec.Command(b.bin, args...)
	cmd.Env = b.env()
	start := time.Now()
	out, err := cmd.CombinedOutput()
	m := measured{Output: string(out), Err: err, Wall: time.Since(start), ExitOK: err == nil}
	if ps := cmd.ProcessState; ps != nil {
		m.MaxRSS = maxRSSBytes(ps)
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			m.User = timeval(ru.Utime)
			m.Sys = timeval(ru.Stime)
			m.DiskIn = int64(ru.Inblock)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(m.Output), "{") {
		var env map[string]any
		if json.Unmarshal([]byte(m.Output), &env) == nil {
			m.JSONEnv = env
		}
	}
	return m
}

func timeval(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}

// cli runs the binary and requires exit status 0.
func (b *benchEnv) cli(args ...string) measured {
	b.t.Helper()
	m := b.run(args...)
	if m.Err != nil {
		b.t.Fatalf("dustkeeper %s: %v\n%s", strings.Join(args, " "), m.Err, m.Output)
	}
	return m
}

// ---- envelope access ------------------------------------------------------
//
// Every number below is required. The previous version read the envelope with a
// helper that returned zero for a missing key, which is how "pending" and "epoch"
// were read from keys that do not exist: the report printed 0, the assertions
// compared 0 with 0, and the evidence looked like a clean run. A missing key is a
// failure now.

func dataOf(t *testing.T, m measured) map[string]any {
	t.Helper()
	data, ok := m.JSONEnv["data"].(map[string]any)
	if !ok {
		t.Fatalf("the envelope has no data object: %s", strings.TrimSpace(m.Output))
	}
	return data
}

func mustObj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("the envelope has no %q object (keys: %v)", key, keysOf(m))
	}
	return o
}

func mustNum(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			t.Fatalf("%q is not an integer: %v", key, err)
		}
		return n
	}
	t.Fatalf("the envelope has no numeric %q (keys: %v)", key, keysOf(m))
	return 0
}

func mustBool(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key].(bool)
	if !ok {
		t.Fatalf("the envelope has no boolean %q (keys: %v)", key, keysOf(m))
	}
	return v
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- tree shape -----------------------------------------------------------

// sizePool is the size distribution. It is deliberately skewed towards small
// files: the interesting question is whether per-file cost grows, and a tree of
// 1 000 000 large files would mostly measure the disk.
var sizePool = []int64{32, 64, 128, 512, 2048, 8192}

type treeShape struct {
	Files         int     `json:"files"`
	LeafDirs      int     `json:"leaf_dirs"`
	Dirs          int     `json:"directories"`
	MaxFanout     int     `json:"max_directory_fanout"`
	TotalBytes    int64   `json:"total_bytes"`
	DistinctSizes int     `json:"distinct_sizes"`
	MeanSizeBytes float64 `json:"mean_file_bytes"`
}

// populate writes the tree and returns a description of what it wrote.
//
// The files are ordinary files with deterministic contents. Nothing in the tree
// exists to measure a duplicate index: that capability is gone, so a benchmark
// that still counted duplicate groups would be the last place in the repository
// claiming it exists.
func populate(t *testing.T, root string, files, leafDirs int) treeShape {
	t.Helper()
	if leafDirs <= 0 {
		leafDirs = 1
	}
	shape := treeShape{Files: files, LeafDirs: leafDirs, DistinctSizes: len(sizePool)}

	// Directory fan-out: group leaves under parents so the tree has two levels,
	// which is what a real cache directory looks like.
	const perParent = 20
	parents := (leafDirs + perParent - 1) / perParent
	leaves := make([]string, leafDirs)
	for i := range leaves {
		parent := filepath.Join(root, fmt.Sprintf("group-%03d", i/perParent))
		if i%perParent == 0 {
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", parent, err)
			}
		}
		leaves[i] = filepath.Join(parent, fmt.Sprintf("batch-%04d", i))
		if err := os.MkdirAll(leaves[i], 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", leaves[i], err)
		}
	}
	shape.Dirs = parents + leafDirs + 1

	old := time.Now().Add(-90 * 24 * time.Hour)
	var wg sync.WaitGroup
	work := make(chan int, 4096)
	errCh := make(chan error, 1)

	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				size := sizePool[i%len(sizePool)]
				p := filepath.Join(leaves[i%leafDirs], fmt.Sprintf("file-%07d.zip", i))
				if err := os.WriteFile(p, content(i, size), 0o600); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				_ = os.Chtimes(p, old, old)
			}
		}()
	}
	for i := 0; i < files; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("populate: %v", err)
	default:
	}

	// The facts a reader needs to judge the numbers: how big and how wide.
	perDir := make(map[string]int, leafDirs)
	var total int64
	for i := 0; i < files; i++ {
		total += sizePool[i%len(sizePool)]
		perDir[leaves[i%leafDirs]]++
	}
	maxFanout := 0
	for _, n := range perDir {
		if n > maxFanout {
			maxFanout = n
		}
	}
	shape.MaxFanout = maxFanout
	shape.TotalBytes = total
	if files > 0 {
		shape.MeanSizeBytes = float64(total) / float64(files)
	}
	return shape
}

// content returns deterministic bytes for an index. Two runs create the same
// bytes, so a size difference between them is a bug rather than noise.
func content(seed int, size int64) []byte {
	if size <= 0 {
		return nil
	}
	body := make([]byte, size)
	stamp := []byte(fmt.Sprintf("dk:%08d:", seed))
	for i := range body {
		body[i] = stamp[i%len(stamp)]
	}
	return body
}

// ---- evidence -------------------------------------------------------------

// evidence is one report row. Every field is produced by a run; none is typed by
// hand, which is the difference between this and a claim.
type evidence struct {
	GeneratedAt string `json:"generated_at"`
	// Commit is recorded with the numbers, because a performance figure without
	// the tree it was measured on is a figure about nothing in particular. It is
	// read from git and reported as "unknown" when that fails: a written-in
	// constant would be worse than an honest gap.
	Commit       string           `json:"commit"`
	Host         string           `json:"host"`
	GoVersion    string           `json:"go_version"`
	FilesFromEnv int              `json:"requested_files"`
	Shape        treeShape        `json:"tree"`
	Scan         scanFacts        `json:"full_scan"`
	DefaultRun   defaultRunFacts  `json:"default_budget_run"`
	Resume       resumeFacts      `json:"resume"`
	Limits       map[string]int64 `json:"limits"`
}

type scanFacts struct {
	WallMS      int64 `json:"wall_ms"`
	UserMS      int64 `json:"cpu_user_ms"`
	SysMS       int64 `json:"cpu_sys_ms"`
	MaxRSSBytes int64 `json:"peak_rss_bytes"`
	// DiskInOps is Rusage inblock as the kernel reports it: a count of filesystem
	// input operations. The manual says operations, so multiplying it by 512 would
	// manufacture bytes out of a number that is not measured in them; the unit is
	// kept and named instead. It is a block-layer counter either way: with a warm
	// page cache it stays near zero even though the scan read every directory.
	DiskInOps int64 `json:"inblock_ops"`
	// ContentBytesRead is null, not zero: the scan opens no file content, so
	// there is no measurement to report. Zero would read like one.
	ContentBytesRead *int64 `json:"content_bytes_read"`

	Epoch            int64   `json:"epoch"`
	DirsScanned      int     `json:"dirs_scanned"`
	FilesScanned     int     `json:"files_scanned"`
	MetadataBytes    int64   `json:"metadata_bytes"`
	ProtectedDirs    int     `json:"protected_dirs"`
	SkippedEntries   int     `json:"skipped_entries"`
	Unreadable       int     `json:"unreadable"`
	VanishedEntries  int     `json:"vanished_entries"`
	FailedDirs       int     `json:"failed_dirs"`
	QueuePending     int64   `json:"queue_pending_after"`
	EpochComplete    bool    `json:"epoch_complete"`
	Interrupted      bool    `json:"interrupted"`
	DBBytesInitial   int64   `json:"db_bytes_initial"`
	DBBytesBefore    int64   `json:"db_bytes_before_scan"`
	DBBytesAfter     int64   `json:"db_bytes_after_scan"`
	DBGrowthBytes    int64   `json:"db_growth_bytes"`
	Observations     int64   `json:"observation_rows"`
	BytesPerFile     float64 `json:"db_bytes_per_file"`
	WALBytes         any     `json:"wal_bytes"`
	WALApplicability string  `json:"wal_note"`
}

// defaultRunFacts is one run under the configured default budget: the scheduled
// path, not the wide-open one. It is a separate row because "the tree can be
// scanned" and "one scheduled run behaves" are different claims.
type defaultRunFacts struct {
	BudgetSeconds int    `json:"run_soft_seconds"`
	WallMS        int64  `json:"wall_ms"`
	UserMS        int64  `json:"cpu_user_ms"`
	SysMS         int64  `json:"cpu_sys_ms"`
	MaxRSSBytes   int64  `json:"peak_rss_bytes"`
	Scanned       int    `json:"scanned_files"`
	ScanComplete  bool   `json:"scan_epoch_complete"`
	Interrupted   bool   `json:"scan_interrupted"`
	DeepScan      bool   `json:"deep_scan_required"`
	Control       string `json:"control"`
	// Code is the envelope code this run ended with. It is recorded so that the
	// row cannot be read as a measurement when the run actually failed: only OK
	// and PARTIAL describe work the budget stopped.
	Code          string `json:"code"`
	QuarantineMsg string `json:"quarantine_skipped,omitempty"`
}

type resumeFacts struct {
	Batches          int     `json:"batches"`
	FilesAcrossBatch int     `json:"files_scanned_total"`
	OverReadFactor   float64 `json:"files_scanned_over_files"`
	MaxRSSBytes      int64   `json:"peak_rss_bytes"`
	LongestBatchMS   int64   `json:"longest_batch_ms"`
	FinalComplete    bool    `json:"final_epoch_complete"`
}

func maxRSSBytes(ps *os.ProcessState) int64 {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return ru.Maxrss // bytes on darwin
	}
	return ru.Maxrss * 1024 // kilobytes elsewhere
}

func dirBytes(t *testing.T, paths ...string) int64 {
	t.Helper()
	var total int64
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// walFacts reports whether a WAL exists at all. The store fixes
// journal_mode=DELETE, so the honest value for "WAL bytes" is "not applicable"
// and the presence of the file is the run-produced evidence for saying so.
func walFacts(t *testing.T, dbPath string) (any, string) {
	t.Helper()
	if info, err := os.Stat(dbPath + "-wal"); err == nil {
		return info.Size(), "存在 -wal 文件，按实测值记录"
	}
	return nil, "存储固定 journal_mode=DELETE，没有 WAL 文件，因此 WAL 字节不适用"
}

// setRunSoftSeconds rewrites the runtime budget. It is how the interrupted-batch
// case is produced without a test-only hook: a five-second slice is a real budget,
// and the binary has to stop at a safe boundary because of it.
func (b *benchEnv) setRunSoftSeconds(seconds int) {
	b.t.Helper()
	path := filepath.Join(b.config, "config.json")
	body, err := os.ReadFile(path)
	if err != nil {
		b.t.Fatalf("read config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		b.t.Fatalf("parse config: %v", err)
	}
	rt, ok := cfg["runtime"].(map[string]any)
	if !ok {
		b.t.Fatalf("config has no runtime object: %s", body)
	}
	rt["run_soft_seconds"] = seconds
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		b.t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		b.t.Fatalf("write config: %v", err)
	}
}

// ---- the two benchmarks ---------------------------------------------------

// TestSmallTreeScan is the regression that runs first: a tree small enough to be
// scanned in one batch proves the pipeline agrees with itself before a large one
// measures anything.
func TestSmallTreeScan(t *testing.T) {
	b := newBenchEnv(t)
	if b.files > smallFiles {
		// The large case is TestLargeTreeScanEvidence; a small-tree check on a
		// 100 000 tree would not be small.
		b.files = smallFiles
	}
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}
	shape := populate(t, root, b.files, smallFanout)
	b.cli("init")
	b.cli("scope", "add", "--path", root, "--kind", "assisted", "--name", "bench")

	initial := dirBytes(t, filepath.Join(b.state, "state.db"))
	facts := b.fullScanFacts(t, shape, initial)
	if facts.FilesScanned != shape.Files {
		t.Fatalf("scanned %d files, tree has %d", facts.FilesScanned, shape.Files)
	}
	if !facts.EpochComplete || facts.QueuePending != 0 || facts.FailedDirs != 0 {
		t.Fatalf("a small tree did not finish cleanly: %+v", facts)
	}
	if facts.MaxRSSBytes > benchLimit(t) {
		t.Fatalf("small tree peak RSS %d exceeds %d", facts.MaxRSSBytes, benchLimit(t))
	}
}

// TestLargeTreeScanEvidence produces the section 5.12 table.
func TestLargeTreeScanEvidence(t *testing.T) {
	b := newBenchEnv(t)
	leafDirs := defaultFanout
	if v := os.Getenv("DUSTKEEPER_BENCH_FANOUT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("DUSTKEEPER_BENCH_FANOUT=%q is not a positive integer", v)
		}
		leafDirs = n
	}

	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}
	start := time.Now()
	shape := populate(t, root, b.files, leafDirs)
	t.Logf("created %d files in %s: %d leaves, max fan-out %d, %.1f B mean",
		shape.Files, time.Since(start).Round(time.Millisecond), shape.LeafDirs,
		shape.MaxFanout, shape.MeanSizeBytes)

	b.cli("init")
	b.cli("scope", "add", "--path", root, "--kind", "assisted", "--name", "bench")

	ev := evidence{
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Commit:       repoCommit(),
		Host:         runtime.GOOS + "/" + runtime.GOARCH,
		GoVersion:    runtime.Version(),
		FilesFromEnv: b.files,
		Shape:        shape,
		Limits:       map[string]int64{"max_rss_bytes": benchLimit(t)},
	}
	// The empty database is measured before anything is scanned, so "how much did
	// recording this tree cost" has a baseline that is not already full.
	initial := dirBytes(t, filepath.Join(b.state, "state.db"))
	ev.Resume = b.resumeFacts(t, shape)
	ev.Scan = b.fullScanFacts(t, shape, initial)
	ev.DefaultRun = b.defaultBudgetRun(t)

	// The scan ceiling is the section 15 target and it must not be relaxed to
	// make the table look good: a scan whose memory grows with the file count is
	// a failed scan.
	if ev.Scan.MaxRSSBytes > ev.Limits["max_rss_bytes"] {
		t.Errorf("scan peak RSS %d bytes exceeds the %d byte ceiling; a scan must not grow with the file count",
			ev.Scan.MaxRSSBytes, ev.Limits["max_rss_bytes"])
	}
	if ev.DefaultRun.MaxRSSBytes > ev.Limits["max_rss_bytes"] {
		t.Errorf("default-budget run peak RSS %d bytes exceeds the %d byte ceiling",
			ev.DefaultRun.MaxRSSBytes, ev.Limits["max_rss_bytes"])
	}

	writeEvidence(t, ev)
}

func benchLimit(t *testing.T) int64 {
	t.Helper()
	limit := int64(defaultMaxRSS) << 20
	if v := os.Getenv("DUSTKEEPER_BENCH_MAX_RSS_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("DUSTKEEPER_BENCH_MAX_RSS_MB=%q is not a positive integer", v)
		}
		limit = n << 20
	}
	return limit
}

// defaultBudgetRun performs one ordinary run under the configured budget and
// records what it managed to do. It is allowed to stop early — that is what the
// budget is for — but it has to say which of the two things happened.
func (b *benchEnv) defaultBudgetRun(t *testing.T) defaultRunFacts {
	t.Helper()
	// The shipped default, not a test value: the point of this row is what an
	// unattended installation actually does.
	b.setRunSoftSeconds(60)
	defer b.setRunSoftSeconds(60)

	var facts defaultRunFacts
	facts.BudgetSeconds = 60
	m := b.run("run", "--json")
	if m.Err != nil && m.JSONEnv == nil {
		t.Fatalf("default-budget run: %v\n%s", m.Err, m.Output)
	}
	// A run that ended on a real failure can still print a partial result, and a
	// partial result still has a scanned count. Timing it would then record a
	// failure as performance evidence, so the same allowed codes as the resume
	// batches apply.
	facts.Code = acceptableRunCode(t, "default-budget run", m)
	data := dataOf(t, m)
	scopes, ok := data["scopes"].([]any)
	if !ok || len(scopes) != 1 {
		t.Fatalf("expected exactly one scope in the run report, got %v", data["scopes"])
	}
	sr, ok := scopes[0].(map[string]any)
	if !ok {
		t.Fatalf("the scope entry is not an object: %v", scopes[0])
	}
	facts.WallMS = m.Wall.Milliseconds()
	facts.UserMS = m.User.Milliseconds()
	facts.SysMS = m.Sys.Milliseconds()
	facts.MaxRSSBytes = m.MaxRSS
	facts.Scanned = int(mustNum(t, sr, "scanned_files"))
	facts.ScanComplete = mustBool(t, sr, "scan_epoch_complete")
	facts.Interrupted = mustBool(t, sr, "scan_interrupted")
	facts.DeepScan = mustBool(t, sr, "deep_scan_required")
	// control is omitted when it is empty, so an absent value is reported as
	// "not reported" rather than invented.
	facts.Control = strOr(sr, "control", "(not reported)")
	if s, ok := sr["quarantine_skipped"].(string); ok {
		facts.QuarantineMsg = s
	}
	t.Logf("default-budget run: %d files in %d ms, scan complete=%v interrupted=%v control=%s, peak RSS %.1f MiB",
		facts.Scanned, facts.WallMS, facts.ScanComplete, facts.Interrupted, facts.Control,
		float64(facts.MaxRSSBytes)/(1<<20))
	if facts.Scanned == 0 {
		t.Fatal("the default-budget run scanned nothing at all")
	}
	return facts
}

// acceptableRunCode fails unless the run ended on a code that still describes
// measured work. OK is a complete run and PARTIAL is a run the budget or the scan
// state cut short; anything else is a failure that happened to print numbers.
func acceptableRunCode(t *testing.T, what string, m measured) string {
	t.Helper()
	code, _ := m.JSONEnv["code"].(string)
	if code != "OK" && code != "PARTIAL" {
		t.Fatalf("%s ended with %q (%v):\n%s", what, code, m.Err, m.Output)
	}
	return code
}

// strOr reads an optional string. It is used only for fields the envelope
// deliberately omits when empty; a field the evidence depends on uses mustStr.
func strOr(m map[string]any, key, fallback string) string {
	if s, ok := m[key].(string); ok && s != "" {
		return s
	}
	return fallback
}

func mustStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("the envelope has no string %q (keys: %v)", key, keysOf(m))
	}
	return s
}

// resumeFacts drives the scope with a five-second budget until the epoch
// completes, and reports whether every batch made progress and whether the total
// work stayed proportionate to the tree.
func (b *benchEnv) resumeFacts(t *testing.T, shape treeShape) resumeFacts {
	t.Helper()
	// 5 seconds is the smallest budget the configuration accepts. It is a real
	// budget, not a test hook: the binary stops because its slice is over.
	b.setRunSoftSeconds(5)
	defer b.setRunSoftSeconds(60)

	var facts resumeFacts
	const maxBatches = 400
	for batch := 1; batch <= maxBatches; batch++ {
		m := b.run("scan", "--scope", "bench", "--json")
		if m.Err != nil && m.JSONEnv == nil {
			t.Fatalf("batch %d: %v\n%s", batch, m.Err, m.Output)
		}
		acceptableRunCode(t, fmt.Sprintf("batch %d", batch), m)
		data := dataOf(t, m)
		scan := mustObj(t, data, "scan")
		state := mustObj(t, data, "scan_state")
		files := mustNum(t, scan, "files")
		facts.Batches = batch
		facts.FilesAcrossBatch += int(files)
		if m.Wall.Milliseconds() > facts.LongestBatchMS {
			facts.LongestBatchMS = m.Wall.Milliseconds()
		}
		if m.MaxRSS > facts.MaxRSSBytes {
			facts.MaxRSSBytes = m.MaxRSS
		}
		if batch == 1 && files == 0 {
			t.Fatalf("the first budgeted batch scanned no files at all; the budget is not the reason it stopped")
		}
		if mustBool(t, state, "complete") {
			facts.FinalComplete = true
			break
		}
	}
	if !facts.FinalComplete {
		t.Fatalf("the epoch never completed in %d batches; a resume that cannot finish is not a resume", facts.Batches)
	}
	if shape.Files > 0 {
		facts.OverReadFactor = float64(facts.FilesAcrossBatch) / float64(shape.Files)
	}
	// A resumed batch may re-list a directory it had open, but re-reading the
	// whole tree on every batch would mean the queue is not being consumed.
	if facts.OverReadFactor > 3 {
		t.Errorf("resuming scanned %d file entries for a tree of %d (%.2fx); the queue is not converging",
			facts.FilesAcrossBatch, shape.Files, facts.OverReadFactor)
	}
	t.Logf("resume: %d batch(es), %d file entries scanned in total (%.2fx of the tree), longest batch %d ms, peak RSS %.1f MiB",
		facts.Batches, facts.FilesAcrossBatch, facts.OverReadFactor,
		facts.LongestBatchMS, float64(facts.MaxRSSBytes)/(1<<20))
	return facts
}

// fullScanFacts is the row a reviewer checks first: the epoch completed, the
// record count matches the tree, and the resources are the ones reported.
func (b *benchEnv) fullScanFacts(t *testing.T, shape treeShape, initialDB int64) scanFacts {
	t.Helper()
	// The complete-epoch measurement gets the largest budget the configuration
	// accepts. A run that stops because of its own slice would measure the slice
	// rather than the tree, and "complete" is exactly what this row is about.
	b.setRunSoftSeconds(600)
	defer b.setRunSoftSeconds(60)

	db := filepath.Join(b.state, "state.db")
	before := dirBytes(t, db)

	m := b.cli("scan", "--scope", "bench", "--json")
	data := dataOf(t, m)
	scan := mustObj(t, data, "scan")
	state := mustObj(t, data, "scan_state")

	var facts scanFacts
	facts.WallMS = m.Wall.Milliseconds()
	facts.UserMS = m.User.Milliseconds()
	facts.SysMS = m.Sys.Milliseconds()
	facts.MaxRSSBytes = m.MaxRSS
	facts.DiskInOps = m.DiskIn
	facts.DirsScanned = int(mustNum(t, scan, "dirs"))
	facts.FilesScanned = int(mustNum(t, scan, "files"))
	facts.MetadataBytes = mustNum(t, scan, "bytes")
	facts.ProtectedDirs = int(mustNum(t, scan, "protected_dirs"))
	facts.SkippedEntries = int(mustNum(t, scan, "skipped_entries"))
	facts.Unreadable = int(mustNum(t, scan, "unreadable"))
	facts.VanishedEntries = int(mustNum(t, scan, "vanished_entries"))
	facts.FailedDirs = int(mustNum(t, scan, "failed_dirs"))
	facts.EpochComplete = mustBool(t, state, "complete")
	facts.Interrupted = mustBool(t, state, "interrupted")
	facts.QueuePending = mustNum(t, state, "queue_pending")
	facts.Epoch = mustNum(t, state, "epoch")
	facts.DBBytesInitial = initialDB
	facts.DBBytesBefore = before
	facts.DBBytesAfter = dirBytes(t, db)
	facts.DBGrowthBytes = facts.DBBytesAfter - initialDB
	facts.WALBytes, facts.WALApplicability = walFacts(t, db)

	stats := mustObj(t, dataOf(t, b.cli("status", "--json")), "stats")
	facts.Observations = mustNum(t, stats, "observations")

	if shape.Files > 0 {
		facts.BytesPerFile = float64(facts.DBBytesAfter) / float64(shape.Files)
	}

	t.Logf("scan: %d files, %d dirs in %d ms (cpu %.0f+%.0f ms), peak RSS %.1f MiB, db %.1f MiB (%.0f B/file)",
		facts.FilesScanned, facts.DirsScanned, facts.WallMS,
		float64(facts.UserMS), float64(facts.SysMS),
		float64(facts.MaxRSSBytes)/(1<<20),
		float64(facts.DBBytesAfter)/(1<<20), facts.BytesPerFile)

	if !facts.EpochComplete {
		t.Fatalf("the epoch is not complete after a full scan (%+v)", facts)
	}
	if facts.QueuePending != 0 || facts.FailedDirs != 0 {
		t.Fatalf("the queue is not drained: pending %d, failed %d", facts.QueuePending, facts.FailedDirs)
	}
	if facts.FilesScanned != shape.Files {
		t.Fatalf("scanned %d files, tree has %d; the workload was not the one that was reported",
			facts.FilesScanned, shape.Files)
	}
	if facts.Observations != int64(shape.Files) {
		t.Fatalf("observation rows %d for %d files", facts.Observations, shape.Files)
	}
	return facts
}

// repoCommit reports the commit the numbers were measured on. Short, and
// "unknown" rather than a guess: an evidence file that cannot be traced back to a
// tree is not evidence of anything.
func repoCommit() string {
	root, err := repoRoot()
	if err != nil {
		return "unknown"
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// repoRoot walks up from the working directory to the directory holding go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// writeEvidence records the numbers. A path in DUSTKEEPER_BENCH_OUT gets a
// markdown table plus a JSON sidecar; otherwise the numbers stay in the test log.
func writeEvidence(t *testing.T, ev evidence) {
	t.Helper()
	out := os.Getenv("DUSTKEEPER_BENCH_OUT")
	if out == "" {
		t.Log("set DUSTKEEPER_BENCH_OUT=<file.md> to persist these numbers")
		return
	}
	// A relative path is relative to the repository root, not to this package:
	// `go test` runs here, so "docs/evidence/x.md" would otherwise land inside
	// test/bench and nobody would find it.
	if !filepath.IsAbs(out) {
		root, err := repoRoot()
		if err != nil {
			t.Fatalf("locate the repository root: %v", err)
		}
		out = filepath.Join(root, out)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		t.Fatalf("create evidence directory: %v", err)
	}
	body, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if err := os.WriteFile(strings.TrimSuffix(out, ".md")+".json", append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence json: %v", err)
	}
	if err := os.WriteFile(out, []byte(markdown(ev)), 0o644); err != nil {
		t.Fatalf("write evidence markdown: %v", err)
	}
	t.Logf("wrote %s and %s", out, strings.TrimSuffix(out, ".md")+".json")
}

func markdown(ev evidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- generated by test/bench; do not edit by hand -->\n")
	fmt.Fprintf(&b, "# 性能证据：%s 文件的扫描与续扫（%s）\n\n", humanInt(ev.Shape.Files), ev.Host)
	fmt.Fprintf(&b, "生成于 %s，commit %s，Go %s，单进程 bench（真实二进制，非 mock）。\n\n",
		ev.GeneratedAt, ev.Commit, ev.GoVersion)

	b.WriteString("## 1. 工作量（先证明扫完了）\n\n")
	b.WriteString("| 项目 | 数值 |\n|---|---|\n")
	row := func(k, v string) { fmt.Fprintf(&b, "| %s | %s |\n", k, v) }
	row("文件数", humanInt(ev.Shape.Files))
	row("目录数 / 叶子目录", fmt.Sprintf("%s / %s", humanInt(ev.Shape.Dirs), humanInt(ev.Shape.LeafDirs)))
	row("目录最大 fan-out", humanInt(ev.Shape.MaxFanout))
	row("总字节 / 平均文件大小", fmt.Sprintf("%s / %.0f B", humanBytes(ev.Shape.TotalBytes), ev.Shape.MeanSizeBytes))
	row("不同大小档位", humanInt(ev.Shape.DistinctSizes))
	row("完整扫描：观察到文件", humanInt(ev.Scan.FilesScanned))
	row("完整扫描：遍历目录", humanInt(ev.Scan.DirsScanned))
	row("完整扫描：元数据字节", humanBytes(ev.Scan.MetadataBytes))
	row("完整扫描：epoch", fmt.Sprintf("%d（完整：%s）", ev.Scan.Epoch, yesNo(ev.Scan.EpochComplete)))
	row("完整扫描：扫描后队列 pending / failed 目录", fmt.Sprintf("%d / %d", ev.Scan.QueuePending, ev.Scan.FailedDirs))
	row("完整扫描：受保护 / 跳过 / 不可读 / 已消失条目",
		fmt.Sprintf("%d / %d / %d / %d", ev.Scan.ProtectedDirs, ev.Scan.SkippedEntries, ev.Scan.Unreadable, ev.Scan.VanishedEntries))
	row("有效观察记录（observation 行）", humanInt(int(ev.Scan.Observations)))

	b.WriteString("\n## 2. 资源\n\n| 项目 | 数值 |\n|---|---|\n")
	row("wall（完整扫描）", fmt.Sprintf("%d ms", ev.Scan.WallMS))
	row("CPU user+sys", fmt.Sprintf("%d + %d ms", ev.Scan.UserMS, ev.Scan.SysMS))
	row("峰值 RSS", humanBytes(ev.Scan.MaxRSSBytes))
	row("上限", humanBytes(ev.Limits["max_rss_bytes"]))
	row("初始库大小（任何扫描前）", humanBytes(ev.Scan.DBBytesInitial))
	row("完整扫描前 / 后的库大小", fmt.Sprintf("%s / %s", humanBytes(ev.Scan.DBBytesBefore), humanBytes(ev.Scan.DBBytesAfter)))
	row("数据库增长（相对初始）", humanBytes(ev.Scan.DBGrowthBytes))
	row("每文件数据库占用", fmt.Sprintf("%.0f B", ev.Scan.BytesPerFile))
	row("内核块层输入操作（Rusage inblock，单位：次）", humanCount(ev.Scan.DiskInOps))
	row("WAL 字节", nullOrInt(ev.Scan.WALBytes))
	b.WriteString("\n")
	b.WriteString("口径说明：`inblock_ops` 是 getrusage 的 `ru_inblock` 原值，手册把它定义为文件系统输入**操作次数**，不是字节数，所以只记次数、不乘 512 冒充字节；扫描只读元数据时它可能接近 0；")
	b.WriteString("`content_bytes_read` 为 null——扫描不打开文件内容，因此没有可测值，0 会被误读成测量结果；")
	fmt.Fprintf(&b, "%s。\n", ev.Scan.WALApplicability)

	b.WriteString("\n## 3. 默认预算下的一轮运行\n\n| 项目 | 数值 |\n|---|---|\n")
	row("run_soft_seconds", humanInt(ev.DefaultRun.BudgetSeconds))
	row("扫描文件", humanInt(ev.DefaultRun.Scanned))
	row("扫描 epoch 完整", yesNo(ev.DefaultRun.ScanComplete))
	row("扫描被预算中断 / 仍需深度扫描", fmt.Sprintf("%s / %s", yesNo(ev.DefaultRun.Interrupted), yesNo(ev.DefaultRun.DeepScan)))
	row("control", ev.DefaultRun.Control)
	row("wall / CPU user+sys", fmt.Sprintf("%d ms / %d+%d ms", ev.DefaultRun.WallMS, ev.DefaultRun.UserMS, ev.DefaultRun.SysMS))
	row("峰值 RSS", humanBytes(ev.DefaultRun.MaxRSSBytes))
	if ev.DefaultRun.QuarantineMsg != "" {
		row("未新建隔离的原因", ev.DefaultRun.QuarantineMsg)
	}

	b.WriteString("\n## 4. 中断续跑\n\n| 项目 | 数值 |\n|---|---|\n")
	row("批次数", humanInt(ev.Resume.Batches))
	row("各批累计扫描文件条目", humanInt(ev.Resume.FilesAcrossBatch))
	row("相对树大小的倍数", fmt.Sprintf("%.2fx", ev.Resume.OverReadFactor))
	row("最长单批 wall", fmt.Sprintf("%d ms", ev.Resume.LongestBatchMS))
	row("各批峰值 RSS", humanBytes(ev.Resume.MaxRSSBytes))
	row("最终 epoch 完整", yesNo(ev.Resume.FinalComplete))

	b.WriteString("\n扫描上限取自 `DUSTKEEPER_BENCH_MAX_RSS_MB`（默认 256 MiB），突破即失败，不放宽门槛。\n")
	return b.String()
}

func nullOrInt(v any) string {
	switch x := v.(type) {
	case nil:
		return "null（不适用）"
	case int64:
		return humanBytes(x)
	case float64:
		return humanBytes(int64(x))
	case string:
		return "null（" + x + "）"
	}
	return fmt.Sprint(v)
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func humanInt(n int) string {
	s := strconv.Itoa(n)
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	return strings.Join(out, ",")
}

// humanCount formats an operation count. Not humanBytes: inblock counts
// operations, and dressing a count up as a size is the mistake this replaced.
func humanCount(n int64) string {
	return fmt.Sprintf("%d ops", n)
}

func humanBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/kib)
	}
	return fmt.Sprintf("%d B", n)
}
