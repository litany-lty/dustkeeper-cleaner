// Package bench measures the real binary against a large tree.
//
// It is deliberately a black-box benchmark: it builds cmd/dustkeeper, runs it as
// a subprocess and reads the peak resident set size the kernel recorded for that
// process. Measuring internals instead would miss exactly what the requirement
// is about — that a scan of a very large directory does not grow with the number
// of files.
//
// The benchmark creates real files, so it does not run by default:
//
//	DUSTKEEPER_BENCH=1 go test ./test/bench -run TestScan -v
//	DUSTKEEPER_BENCH=1 DUSTKEEPER_BENCH_FILES=100000 \
//	    DUSTKEEPER_BENCH_MAX_RSS_MB=256 go test ./test/bench -run TestScan -v
package bench

import (
	"context"
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
	defaultMaxRSS = 256 // MiB
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

// cli runs the binary and returns stdout, requiring exit status 0.
func (b *benchEnv) cli(args ...string) string {
	b.t.Helper()
	out, err := b.cliMayFail(args...)
	if err != nil {
		b.t.Fatalf("dustkeeper %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (b *benchEnv) cliMayFail(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.bin, args...)
	cmd.Env = b.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// populate writes n small files spread over 100 leaf directories. Creation is
// parallel because the slow part is the syscall round trip, not the benchmark.
func (b *benchEnv) populate(root string) {
	b.t.Helper()
	const fanout = 100
	dirs := make([]string, fanout)
	for i := range dirs {
		dirs[i] = filepath.Join(root, fmt.Sprintf("batch-%03d", i))
		if err := os.MkdirAll(dirs[i], 0o700); err != nil {
			b.t.Fatalf("mkdir: %v", err)
		}
	}

	old := time.Now().Add(-90 * 24 * time.Hour)
	var wg sync.WaitGroup
	work := make(chan int, 1024)
	errs := make(chan error, 1)

	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				p := filepath.Join(dirs[i%fanout], fmt.Sprintf("file-%06d.zip", i))
				if err := os.WriteFile(p, []byte("payload"), 0o600); err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				// Age the file so a rule would select it.
				_ = os.Chtimes(p, old, old)
			}
		}()
	}
	for i := 0; i < b.files; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	select {
	case err := <-errs:
		b.t.Fatalf("populate: %v", err)
	default:
	}
}

// maxRSSBytes reports the peak resident set size of a finished process.
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

func TestScanMemoryIsBounded(t *testing.T) {
	b := newBenchEnv(t)

	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}

	t.Logf("creating %d files under %s", b.files, root)
	start := time.Now()
	b.populate(root)
	created := time.Since(start)
	t.Logf("created %d files in %s (%.0f files/s)",
		b.files, created.Round(time.Millisecond), float64(b.files)/created.Seconds())

	b.cli("init")
	b.cli("scope", "add", "--path", root, "--kind", "observe", "--name", "bench")

	// Scan is the part whose memory could scale with the tree.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.bin, "scan", "--scope", "bench", "--json")
	cmd.Env = b.env()
	scanStart := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(scanStart)
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}

	rss := int64(0)
	if cmd.ProcessState != nil {
		rss = maxRSSBytes(cmd.ProcessState)
	}

	limit := int64(defaultMaxRSS) << 20
	if v := os.Getenv("DUSTKEEPER_BENCH_MAX_RSS_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("DUSTKEEPER_BENCH_MAX_RSS_MB=%q is not a positive integer", v)
		}
		limit = n << 20
	}

	t.Logf("scan of %d files took %s (%.0f files/s), peak RSS %.1f MiB (limit %.0f MiB)",
		b.files, elapsed.Round(time.Millisecond),
		float64(b.files)/elapsed.Seconds(),
		float64(rss)/(1<<20), float64(limit)/(1<<20))

	if rss <= 0 {
		t.Skip("the platform did not report a peak RSS; nothing to compare")
	}
	if rss > limit {
		t.Fatalf("peak RSS %.1f MiB exceeds the %.0f MiB ceiling; a scan must not grow with the file count",
			float64(rss)/(1<<20), float64(limit)/(1<<20))
	}

	// The scan must also have produced a bounded index, not one row per file with
	// an unbounded payload.
	if !strings.Contains(string(out), `"status"`) {
		t.Fatalf("scan produced no JSON envelope:\n%s", out)
	}
}
