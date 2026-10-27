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
//	DUSTKEEPER_BENCH=1 DUSTKEEPER_BENCH_FILES=1000000 \
//	    DUSTKEEPER_BENCH_OUT=docs/evidence/performance-1m.md \
//	    go test ./test/bench -run TestLargeTree -v
//
// It reports the numbers section 5.12 asks for, and writes them to
// DUSTKEEPER_BENCH_OUT (markdown plus a JSON sidecar) when that is set, so the
// compliance table can cite a file that was produced by a run rather than typed
// by hand.
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
	// defaultDupRate is the fraction of files that are byte-identical to an
	// earlier one. It matters for the dedup map peak, which is the one place a
	// full in-memory index is still used.
	defaultDupRate = 0.30
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
		t.Skip("set DUSTKEEPER_BENCH=1 to run the large-tree benchmark")
	}
	if testing.Short() {
		t.Skip("skipping the large-tree benchmark in -short mode")
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
	DiskIn  int64 // bytes the kernel says were read from the block layer
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
			m.DiskIn = int64(ru.Inblock) * 512
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

// ---- tree shape -----------------------------------------------------------

// sizePool is the size distribution. It is deliberately skewed towards small
// files: the interesting question is whether per-file cost grows, and a tree of
// 1 000 000 large files would mostly measure the disk.
var sizePool = []int64{32, 64, 128, 512, 2048, 8192}

type treeShape struct {
	Files          int     `json:"files"`
	LeafDirs       int     `json:"leaf_dirs"`
	Dirs           int     `json:"directories"`
	MaxFanout      int     `json:"max_directory_fanout"`
	TotalBytes     int64   `json:"total_bytes"`
	DistinctSizes  int     `json:"distinct_sizes"`
	DistinctInodes int     `json:"distinct_contents"`
	DupFiles       int     `json:"duplicate_files"`
	DupGroups      int     `json:"duplicate_groups"`
	DupBytes       int64   `json:"duplicate_bytes"`
	MeanSizeBytes  float64 `json:"mean_file_bytes"`
}

// populate writes the tree and returns a description of what it wrote. Content is
// a deterministic function of a seed, so "duplicate" is a fact about the bytes,
// not an assumption the test makes about the detector.
func populate(t *testing.T, root string, files, leafDirs int, dupRate float64) treeShape {
	t.Helper()
	if leafDirs <= 0 {
		leafDirs = 1
	}
	shape := treeShape{Files: files, LeafDirs: leafDirs, DistinctSizes: len(sizePool)}

	distinct := files
	if dupRate > 0 && dupRate < 1 {
		distinct = int(float64(files) * (1 - dupRate))
	}
	if distinct < 1 {
		distinct = 1
	}
	shape.DistinctInodes = distinct

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
				seed := i % distinct
				size := sizePool[seed%len(sizePool)]
				body := content(seed, size)
				p := filepath.Join(leaves[i%leafDirs], fmt.Sprintf("file-%07d.zip", i))
				if err := os.WriteFile(p, body, 0o600); err != nil {
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

	// The facts a reader needs to judge the numbers: how big, how wide, how
	// repetitive.
	perDir := make(map[string]int, leafDirs)
	var total int64
	counts := make(map[string]int, distinct)
	for i := 0; i < files; i++ {
		seed := i % distinct
		size := sizePool[seed%len(sizePool)]
		total += size
		perDir[leaves[i%leafDirs]]++
		counts[fmt.Sprintf("%d:%d", seed, size)]++
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
	for key, n := range counts {
		if n > 1 {
			shape.DupGroups++
			shape.DupFiles += n - 1
			var seed int
			var size int64
			fmt.Sscanf(key, "%d:%d", &seed, &size)
			shape.DupBytes += int64(n-1) * size
		}
	}
	return shape
}

// content returns deterministic bytes for a seed, so two files with the same seed
// are byte-identical and the duplicate count is known before detection runs.
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
	GeneratedAt  string           `json:"generated_at"`
	Host         string           `json:"host"`
	GoVersion    string           `json:"go_version"`
	FilesFromEnv int              `json:"requested_files"`
	Shape        treeShape        `json:"tree"`
	Scan         scanFacts        `json:"full_scan"`
	Resume       resumeFacts      `json:"resume"`
	Dedup        dedupFacts       `json:"dedup"`
	Limits       map[string]int64 `json:"limits"`
}

type scanFacts struct {
	WallMS           int64 `json:"wall_ms"`
	UserMS           int64 `json:"cpu_user_ms"`
	SysMS            int64 `json:"cpu_sys_ms"`
	MaxRSSBytes      int64 `json:"peak_rss_bytes"`
	DiskReadBytes    int64 `json:"os_disk_read_bytes"`
	ContentBytesRead int64 `json:"content_bytes_read"`

	Epoch          int64 `json:"epoch"`
	DirsScanned    int   `json:"dirs_scanned"`
	FilesScanned   int   `json:"files_scanned"`
	MetadataBytes  int64 `json:"metadata_bytes"`
	ProtectedDirs  int   `json:"protected_dirs"`
	SkippedEntries int   `json:"skipped_entries"`
	Unreadable     int   `json:"unreadable"`
	FailedDirs     int   `json:"failed_dirs"`

	EpochComplete bool    `json:"epoch_complete"`
	Pending       int64   `json:"queue_pending_after"`
	Observations  int64   `json:"observation_rows"`
	DBBytes       int64   `json:"db_bytes"`
	WALBytes      int64   `json:"wal_bytes_during"`
	DBGrowthBytes int64   `json:"db_growth_bytes"`
	BytesPerFile  float64 `json:"db_bytes_per_file"`
}

type resumeFacts struct {
	Batches          int     `json:"batches"`
	FilesAcrossBatch int     `json:"files_scanned_total"`
	OverReadFactor   float64 `json:"files_scanned_over_files"`
	MaxRSSBytes      int64   `json:"peak_rss_bytes"`
	LongestBatchMS   int64   `json:"longest_batch_ms"`
	FinalComplete    bool    `json:"final_epoch_complete"`
}

type dedupFacts struct {
	WallMS      int64 `json:"wall_ms"`
	MaxRSSBytes int64 `json:"peak_rss_bytes"`
	Groups      int64 `json:"content_groups"`
	Plannable   int64 `json:"plannable_groups"`
	LogicalByte int64 `json:"logical_bytes"`
	ExpectedDup int   `json:"expected_duplicate_files"`
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

func numOf(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func objOf(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	o, _ := m[key].(map[string]any)
	return o
}

func boolOf(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// setRunSoftSeconds rewrites the runtime budget. It is how the interrupted-batch
// case is produced without a test-only hook: a one-second slice is a real budget,
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

// TestLargeTreeScanEvidence produces the section 5.12 table.
//
// Four questions, in order:
//  1. did the scan actually finish the tree, and how much did it cost?
//  2. after a budget stop, does resuming make progress without re-reading?
//  3. is the resident set still bounded when the tree is very large?
//  4. what does the one remaining in-memory index (dedup) cost?
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
	dupRate := defaultDupRate
	if v := os.Getenv("DUSTKEEPER_BENCH_DUP_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f >= 1 {
			t.Fatalf("DUSTKEEPER_BENCH_DUP_RATE=%q is not a fraction in [0,1)", v)
		}
		dupRate = f
	}

	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}
	start := time.Now()
	shape := populate(t, root, b.files, leafDirs, dupRate)
	t.Logf("created %d files in %s: %d leaves, max fan-out %d, %.1f B mean, %d duplicate files in %d groups",
		shape.Files, time.Since(start).Round(time.Millisecond), shape.LeafDirs, shape.MaxFanout,
		shape.MeanSizeBytes, shape.DupFiles, shape.DupGroups)

	b.cli("init")
	b.cli("scope", "add", "--path", root, "--kind", "assisted", "--name", "bench")

	ev := evidence{
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Host:         runtime.GOOS + "/" + runtime.GOARCH,
		GoVersion:    runtime.Version(),
		FilesFromEnv: b.files,
		Shape:        shape,
		Limits: map[string]int64{
			"max_rss_bytes": benchLimit(t),
		},
	}
	ev.Resume = b.resumeFacts(t, shape)
	ev.Scan = b.fullScanFacts(t, shape)
	ev.Dedup = b.dedupFacts(t, shape)

	// The scan ceiling is the section 15 target and it must not be relaxed to
	// make the table look good: a scan whose memory grows with the file count is
	// a failed scan.
	if ev.Scan.MaxRSSBytes > ev.Limits["max_rss_bytes"] {
		t.Errorf("scan peak RSS %d bytes exceeds the %d byte ceiling; a scan must not grow with the file count",
			ev.Scan.MaxRSSBytes, ev.Limits["max_rss_bytes"])
	}

	// Dedup is measured, not excused. It keeps one candidate per indexed file
	// and one group per duplicate set, so its peak is linear in the tree; the
	// measurement below is the evidence for that statement. Section 15's 256 MiB
	// is a scan target, so failing this test against it would conflate two
	// different requirements - but reporting the number without a ceiling would
	// hide a real limit, so an explicit opt-in ceiling exists for anyone who
	// wants the test to enforce one.
	t.Logf("dedup peak RSS %.1f MiB for %d files (%.0f bytes/file); this is linear in the tree, not bounded by the duplicate count",
		float64(ev.Dedup.MaxRSSBytes)/(1<<20), ev.Shape.Files,
		float64(ev.Dedup.MaxRSSBytes)/float64(maxInt(ev.Shape.Files, 1)))
	if v := os.Getenv("DUSTKEEPER_BENCH_DEDUP_MAX_RSS_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("DUSTKEEPER_BENCH_DEDUP_MAX_RSS_MB=%q is not a positive integer", v)
		}
		if ev.Dedup.MaxRSSBytes > n<<20 {
			t.Errorf("dedup peak RSS %.1f MiB exceeds the requested %.0f MiB ceiling",
				float64(ev.Dedup.MaxRSSBytes)/(1<<20), float64(n))
		}
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

// resumeFacts drives the scope with a one-second budget until the epoch
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
		if code, _ := m.JSONEnv["code"].(string); code != "OK" && code != "PARTIAL" {
			t.Fatalf("batch %d ended with %s: %v\n%s", batch, code, m.Err, m.Output)
		}
		scan := objOf(objOf(m.JSONEnv, "data"), "scan")
		state := objOf(objOf(m.JSONEnv, "data"), "scan_state")
		files := numOf(scan, "files")
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
		if boolOf(state, "complete") {
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
func (b *benchEnv) fullScanFacts(t *testing.T, shape treeShape) scanFacts {
	t.Helper()
	// The complete-epoch measurement gets the largest budget the configuration
	// accepts. A run that stops because of its own slice would measure the slice
	// rather than the tree, and "complete" is exactly what this row is about.
	b.setRunSoftSeconds(600)
	defer b.setRunSoftSeconds(60)

	db := filepath.Join(b.state, "state.db")
	wal := db + "-wal"
	beforeDB := dirBytes(t, db)
	beforeWAL := dirBytes(t, wal)

	m := b.cli("scan", "--scope", "bench", "--json")
	scan := objOf(objOf(m.JSONEnv, "data"), "scan")
	state := objOf(objOf(m.JSONEnv, "data"), "scan_state")

	var facts scanFacts
	facts.WallMS = m.Wall.Milliseconds()
	facts.UserMS = m.User.Milliseconds()
	facts.SysMS = m.Sys.Milliseconds()
	facts.MaxRSSBytes = m.MaxRSS
	facts.DiskReadBytes = m.DiskIn
	facts.DirsScanned = int(numOf(scan, "dirs"))
	facts.FilesScanned = int(numOf(scan, "files"))
	facts.MetadataBytes = numOf(scan, "bytes")
	facts.ProtectedDirs = int(numOf(scan, "protected_dirs"))
	facts.SkippedEntries = int(numOf(scan, "skipped_entries"))
	facts.Unreadable = int(numOf(scan, "unreadable"))
	facts.FailedDirs = int(numOf(scan, "failed_dirs"))
	facts.EpochComplete = boolOf(state, "complete")
	facts.Pending = numOf(state, "Pending")
	facts.Epoch = numOf(state, "Epoch")
	facts.WALBytes = dirBytes(t, wal)
	facts.DBBytes = dirBytes(t, db)
	facts.DBGrowthBytes = facts.DBBytes + facts.WALBytes - beforeDB - beforeWAL

	stats := objOf(objOf(b.cli("status", "--json").JSONEnv, "data"), "stats")
	facts.Observations = numOf(stats, "observations")
	if shape.Files > 0 {
		facts.BytesPerFile = float64(facts.DBBytes) / float64(shape.Files)
	}

	t.Logf("scan: %d files, %d dirs in %d ms (cpu %.0f+%.0f ms), peak RSS %.1f MiB, os read %.1f MiB, db %.1f MiB (%.0f B/file)",
		facts.FilesScanned, facts.DirsScanned, facts.WallMS,
		float64(facts.UserMS), float64(facts.SysMS),
		float64(facts.MaxRSSBytes)/(1<<20), float64(facts.DiskReadBytes)/(1<<20),
		float64(facts.DBBytes)/(1<<20), facts.BytesPerFile)

	if !facts.EpochComplete {
		t.Fatalf("the epoch is not complete after a full scan (%+v)", facts)
	}
	if facts.Pending != 0 || facts.FailedDirs != 0 {
		t.Fatalf("the queue is not drained: pending %d, failed %d", facts.Pending, facts.FailedDirs)
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

// dedupFacts measures the one stage that still builds a whole-season index in
// memory. bySize groups are cheap; byHash has to open and read each candidate, so
// this is also the check on "how many bytes does it actually read".
func (b *benchEnv) dedupFacts(t *testing.T, shape treeShape) dedupFacts {
	t.Helper()
	m := b.run("dedup", "plan", "--scope", "bench", "--json")
	if code, _ := m.JSONEnv["code"].(string); code != "OK" && code != "NO_CANDIDATES" {
		t.Fatalf("dedup plan ended with %q: %v\n%s", code, m.Err, m.Output)
	}
	data := objOf(m.JSONEnv, "data")
	facts := dedupFacts{
		WallMS:      m.Wall.Milliseconds(),
		MaxRSSBytes: m.MaxRSS,
		Groups:      numOf(data, "groups"),
		Plannable:   numOf(data, "plannable"),
		LogicalByte: numOf(data, "logical_bytes"),
		ExpectedDup: shape.DupFiles,
	}
	t.Logf("dedup: %d content groups (%d plannable), %.1f MiB logical in %d ms, peak RSS %.1f MiB, expected %d duplicate files",
		facts.Groups, facts.Plannable, float64(facts.LogicalByte)/(1<<20),
		facts.WallMS, float64(facts.MaxRSSBytes)/(1<<20), shape.DupFiles)
	// A tree built without duplicates must produce no group; a tree built with
	// them must produce at least one. Both directions are asserted because a
	// detector that finds nothing would otherwise look fast and correct.
	if shape.DupFiles > 0 && facts.Groups == 0 {
		t.Fatalf("the tree was built with %d duplicate files but dedup found no content group", shape.DupFiles)
	}
	if shape.DupFiles == 0 && facts.Plannable != 0 {
		t.Fatalf("the tree has no duplicate bytes but dedup planned %d copies", facts.Plannable)
	}
	// "How much would this plan free?" must not be answered with zero.
	if facts.Plannable > 0 && facts.LogicalByte == 0 {
		t.Fatalf("%d plannable copies were reported with 0 logical bytes", facts.Plannable)
	}
	return facts
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
	fmt.Fprintf(&b, "# 性能证据：%s 文件的扫描与去重（%s）\n\n", humanInt(ev.Shape.Files), ev.Host)
	fmt.Fprintf(&b, "生成于 %s，Go %s，单进程 bench（真实二进制，非 mock）。\n\n", ev.GeneratedAt, ev.GoVersion)

	b.WriteString("## 1. 工作量（先证明扫完了）\n\n")
	b.WriteString("| 项目 | 数值 |\n|---|---|\n")
	row := func(k, v string) { fmt.Fprintf(&b, "| %s | %s |\n", k, v) }
	row("文件数", humanInt(ev.Shape.Files))
	row("目录数 / 叶子目录", fmt.Sprintf("%s / %s", humanInt(ev.Shape.Dirs), humanInt(ev.Shape.LeafDirs)))
	row("目录最大 fan-out", humanInt(ev.Shape.MaxFanout))
	row("总字节 / 平均文件大小", fmt.Sprintf("%s / %.0f B", humanBytes(ev.Shape.TotalBytes), ev.Shape.MeanSizeBytes))
	row("不同大小档位", humanInt(ev.Shape.DistinctSizes))
	row("重复文件 / 重复组", fmt.Sprintf("%s / %s", humanInt(ev.Shape.DupFiles), humanInt(ev.Shape.DupGroups)))
	row("重复率（按文件数）", fmt.Sprintf("%.1f%%", 100*float64(ev.Shape.DupFiles)/float64(maxInt(ev.Shape.Files, 1))))
	row("完整扫描：观察到文件", humanInt(ev.Scan.FilesScanned))
	row("完整扫描：遍历目录", humanInt(ev.Scan.DirsScanned))
	row("完整扫描：元数据字节", humanBytes(ev.Scan.MetadataBytes))
	row("epoch 完整", yesNo(ev.Scan.EpochComplete))
	row("扫描后队列 pending", humanInt(int(ev.Scan.Pending)))
	row("受保护/跳过/不可读目录", fmt.Sprintf("%d / %d / %d", ev.Scan.ProtectedDirs, ev.Scan.SkippedEntries, ev.Scan.Unreadable))
	row("有效观察记录（observation 行）", humanInt(int(ev.Scan.Observations)))

	b.WriteString("\n## 2. 资源\n\n| 项目 | 数值 |\n|---|---|\n")
	row("wall（完整扫描）", fmt.Sprintf("%d ms", ev.Scan.WallMS))
	row("CPU user+sys", fmt.Sprintf("%d + %d ms", ev.Scan.UserMS, ev.Scan.SysMS))
	row("峰值 RSS", humanBytes(ev.Scan.MaxRSSBytes))
	row("上限", humanBytes(ev.Limits["max_rss_bytes"]))
	row("内核层读取（Rusage inblock）", humanBytes(ev.Scan.DiskReadBytes))
	row("读取文件内容字节", humanBytes(ev.Scan.ContentBytesRead))
	row("SQLite 文件大小", humanBytes(ev.Scan.DBBytes))
	row("WAL 增量（扫描期间）", humanBytes(ev.Scan.WALBytes))
	row("数据库增长", humanBytes(ev.Scan.DBGrowthBytes))
	row("每文件数据库占用", fmt.Sprintf("%.0f B", ev.Scan.BytesPerFile))

	b.WriteString("\n## 3. 中断续跑\n\n| 项目 | 数值 |\n|---|---|\n")
	row("批次数", humanInt(ev.Resume.Batches))
	row("各批累计扫描文件条目", humanInt(ev.Resume.FilesAcrossBatch))
	row("相对树大小的倍数", fmt.Sprintf("%.2fx", ev.Resume.OverReadFactor))
	row("最长单批 wall", fmt.Sprintf("%d ms", ev.Resume.LongestBatchMS))
	row("各批峰值 RSS", humanBytes(ev.Resume.MaxRSSBytes))
	row("最终 epoch 完整", yesNo(ev.Resume.FinalComplete))

	b.WriteString("\n## 4. 去重\n\n| 项目 | 数值 |\n|---|---|\n")
	row("按大小分组（含哈希）/ 可规划组", fmt.Sprintf("%d / %d", ev.Dedup.Groups, ev.Dedup.Plannable))
	row("逻辑字节", humanBytes(ev.Dedup.LogicalByte))
	row("wall", fmt.Sprintf("%d ms", ev.Dedup.WallMS))
	row("峰值 RSS", humanBytes(ev.Dedup.MaxRSSBytes))
	row("预期重复文件", humanInt(ev.Dedup.ExpectedDup))

	b.WriteString("\n扫描上限取自 `DUSTKEEPER_BENCH_MAX_RSS_MB`（默认 256 MiB），突破即失败，")
	b.WriteString("不放宽门槛。去重峰值只报告、不套用扫描门槛：它按设计每个索引文件留一个候选、")
	b.WriteString("每个重复组留两个成员，因此随文件数线性增长（`DUSTKEEPER_BENCH_DEDUP_MAX_RSS_MB` 可显式设限）。")
	b.WriteString("`Rusage inblock` 在 APFS 上常为 0（页面缓存命中，且扫描只读元数据）。\n")
	return b.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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
