package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/review"
)

const (
	jevKey                = "review-smoke-jev-key"
	miniMaxKey            = "review-smoke-minimax-key"
	jevModel              = "jev-latest"
	miniMaxModel          = "MiniMax-Smoke"
	smokePayload          = "smoke payload\n"
	maxCommandOutputBytes = 1 << 20
	commandTimeout        = 10 * time.Second
)

var errCommandOutputTooLarge = errors.New("artifact output exceeds 1 MiB")

func main() {
	artifact := flag.String("artifact", "", "dustkeeper artifact to exercise")
	work := flag.String("work", "", "empty working directory for the smoke")
	flag.Parse()
	if *artifact == "" || *work == "" {
		fatalf("--artifact and --work are required")
	}
	if err := runSmoke(*artifact, *work); err != nil {
		fatalf("%v", err)
	}
	fmt.Println("review smoke: keep retained the source; allowed quarantine restored identical bytes; " +
		"one mixed batch deleted one file, observed one and left one where it was")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "review smoke: "+format+"\n", args...)
	os.Exit(1)
}

func runSmoke(artifact, work string) error {
	artifact, err := filepath.Abs(artifact)
	if err != nil {
		return err
	}
	work, err = prepareWork(work)
	if err != nil {
		return err
	}
	models := &fakeModels{}
	server, caPEM, err := newTLSServer(models)
	if err != nil {
		return err
	}
	defer server.Close()
	caFile := filepath.Join(work, "review-smoke-ca.pem")
	if err := writePrivate(caFile, caPEM); err != nil {
		return err
	}

	models.allow.Store(false)
	if err := runPhase(artifact, filepath.Join(work, "reject"), server.URL, caFile, models, false); err != nil {
		return fmt.Errorf("rejection phase: %w", err)
	}
	models.allow.Store(true)
	if err := runPhase(artifact, filepath.Join(work, "allow"), server.URL, caFile, models, true); err != nil {
		return fmt.Errorf("allow phase: %w", err)
	}
	if err := runMixedBatch(artifact, filepath.Join(work, "mixed"), server.URL, caFile, models); err != nil {
		return fmt.Errorf("mixed batch: %w", err)
	}
	return nil
}

// prepareWork enforces the command-line contract before the helper starts a
// listener or creates any test material. A caller-owned directory is accepted
// only when it is a real, empty directory; symlinks are never followed.
func prepareWork(work string) (string, error) {
	info, err := os.Lstat(work)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(work, 0o700); err != nil {
			return "", fmt.Errorf("create empty work directory: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("inspect work directory: %w", err)
	} else {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("work directory must not be a symbolic link")
		}
		if !info.IsDir() {
			return "", errors.New("work path must be a directory")
		}
		if info.Mode().Perm() != 0o700 {
			return "", errors.New("work directory permissions must be 0700")
		}
		entries, err := os.ReadDir(work)
		if err != nil {
			return "", fmt.Errorf("read work directory: %w", err)
		}
		if len(entries) != 0 {
			return "", errors.New("work directory must be empty")
		}
	}
	realWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		return "", fmt.Errorf("resolve work directory: %w", err)
	}
	return realWork, nil
}

type commandEnv struct {
	artifact string
	state    string
	config   string
	home     string
}

type commandResult struct {
	stdout []byte
	stderr []byte
}

type boundedBuffer struct {
	buffer bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := maxCommandOutputBytes - b.buffer.Len()
	if remaining <= 0 {
		return 0, errCommandOutputTooLarge
	}
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		return remaining, errCommandOutputTooLarge
	}
	return b.buffer.Write(p)
}

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (c commandEnv) run(args ...string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.artifact, args...)
	cmd.Dir = c.home
	cmd.Env = []string{
		"DUSTKEEPER_STATE_DIR=" + c.state,
		"DUSTKEEPER_CONFIG_DIR=" + c.config,
		"HOME=" + c.home,
		"TMPDIR=" + c.home,
		"PATH=/usr/bin:/bin",
	}
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = errors.New("artifact command timed out")
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}, err
}

func (c commandEnv) mustRun(label string, args ...string) (commandResult, error) {
	result, err := c.run(args...)
	if err != nil {
		return result, fmt.Errorf("%s failed: %w; stdout=%s stderr=%s", label, err,
			strings.TrimSpace(string(result.stdout)), strings.TrimSpace(string(result.stderr)))
	}
	return result, nil
}

// install is one private installation of the artifact: its own tree, state
// directory, configuration and home. A phase reads and writes nothing outside
// it, so two phases in one smoke cannot see each other's files or database, and
// nothing the smoke does is visible to an installation on the same machine.
type install struct {
	dir  string
	tree string
	cli  commandEnv
}

func newInstall(artifact, dir, serverURL, caFile string) (install, error) {
	for _, sub := range []string{"tree", "state", "config", "home"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return install{}, err
		}
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return install{}, err
	}
	// No evidence file and no retained copy: the release smoke exercises the
	// ordinary path, where the program measures the facts itself.
	credentials := filepath.Join(dir, "credentials.json")
	if err := writePrivate(credentials, []byte(`{"typesafe_api_key":"`+jevKey+`","minimax_api_key":"`+miniMaxKey+`"}`)); err != nil {
		return install{}, err
	}

	cli := commandEnv{artifact: artifact, state: filepath.Join(dir, "state"), config: filepath.Join(dir, "config"), home: filepath.Join(dir, "home")}
	if _, err := cli.mustRun("init", "init", "--json"); err != nil {
		return install{}, err
	}
	cfg := config.Default()
	// The free-space floor is a property of the host, not of the artifact: a build
	// machine whose volume is nearly full would stop at DISK_PRESSURE before the
	// review path was ever reached, and the smoke would report a disk condition as
	// a broken build. The smoke therefore runs with the lowest floor the
	// configuration permits, and says so, instead of skipping the check.
	cfg.Runtime.MinFreePercent = 1
	cfg.Runtime.MinFreeBytes = 0
	cfg.Review = &review.Settings{
		TrustCAFile: caFile, CredentialsFile: credentials,
		JEVURL: serverURL + "/jev", JEVModel: jevModel,
		MiniMaxURL: serverURL + "/minimax", MiniMaxModel: miniMaxModel,
		MaxCandidates: 10,
	}
	configJSON, err := config.Marshal(cfg)
	if err != nil {
		return install{}, err
	}
	if err := writePrivate(filepath.Join(cli.config, "config.json"), configJSON); err != nil {
		return install{}, err
	}
	return install{dir: dir, tree: filepath.Join(dir, "tree"), cli: cli}, nil
}

func runPhase(artifact, dir, serverURL, caFile string, models *fakeModels, allow bool) error {
	inst, err := newInstall(artifact, dir, serverURL, caFile)
	if err != nil {
		return err
	}
	cli, tree := inst.cli, inst.tree
	source := filepath.Join(tree, "old.zip")
	if err := os.WriteFile(source, []byte(smokePayload), 0o600); err != nil {
		return err
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(source, old, old); err != nil {
		return err
	}
	ruleFile := filepath.Join(inst.dir, "rule.json")
	rule := `{"dsl_version":1,"scope_id":"smoke","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":1073741824,"relative_dir_prefixes":[],"basename_patterns":["*.zip"]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`
	if err := writePrivate(ruleFile, []byte(rule)); err != nil {
		return err
	}
	steps := []struct {
		label string
		args  []string
	}{
		{"scope add", []string{"scope", "add", "--path", tree, "--kind", "managed_disposable", "--name", "smoke", "--json"}},
		{"scan", []string{"scan", "--scope", "smoke", "--json"}},
		{"rule import", []string{"rule", "import", "--file", ruleFile, "--id", "smoke-rule", "--json"}},
	}
	for _, step := range steps {
		if _, err := cli.mustRun(step.label, step.args...); err != nil {
			return err
		}
	}
	preview, err := cli.mustRun("rule preview", "rule", "preview", "--id", "smoke-rule", "--revision", "1", "--json")
	if err != nil {
		return err
	}
	reviewDigest, ok := findString(preview.stdout, "review_digest")
	if !ok {
		return errors.New("rule preview returned no review_digest")
	}
	if _, err := cli.mustRun("rule activate", "rule", "activate", "--id", "smoke-rule", "--revision", "1", "--digest", reviewDigest, "--json"); err != nil {
		return err
	}
	planResult, err := cli.mustRun("plan create", "plan", "create", "--rule", "smoke-rule", "--json")
	if err != nil {
		return err
	}
	planID, idOK := findString(planResult.stdout, "plan_id")
	planDigest, digestOK := findString(planResult.stdout, "digest")
	if !idOK || !digestOK {
		return errors.New("plan create returned no id or digest")
	}
	miniMaxBefore := models.miniMaxCalls.Load()
	apply, applyErr := cli.run("plan", "apply", "--id", planID, "--digest", planDigest, "--json")
	if !allow {
		// A keep is an answer, not a failed run. The three-outcome product reports
		// "the reviewers said no, so nothing happened" as a completed plan with a
		// KEPT result, and the command exits 0 so a scheduler does not log an
		// ordinary refusal as an outage every night.
		if applyErr != nil {
			return fmt.Errorf("a rejection must be reported as a keep, not as a broken run: %w; stdout=%s stderr=%s",
				applyErr, apply.stdout, apply.stderr)
		}
		if !bytes.Contains(apply.stdout, []byte("KEPT")) {
			return fmt.Errorf("rejection was not reported as a keep: stdout=%s stderr=%s", apply.stdout, apply.stderr)
		}
		if models.miniMaxCalls.Load() != miniMaxBefore {
			return errors.New("MiniMax was called after JEV rejected the candidate")
		}
		body, err := os.ReadFile(source)
		if err != nil || string(body) != smokePayload {
			return errors.New("review rejection did not leave the source bytes in place")
		}
		// "Nothing was moved" is observed rather than inferred from the bytes: a
		// payload left in the vault *and* a source left in the tree would satisfy
		// the byte check above and still be a quarantine.
		vault, err := cli.mustRun("quarantine list after rejection", "quarantine", "list", "--json")
		if err != nil {
			return err
		}
		if bytes.Contains(vault.stdout, []byte("item_id")) {
			return fmt.Errorf("a kept file was moved into the observation area: %s", vault.stdout)
		}
		return nil
	}
	if applyErr != nil {
		return fmt.Errorf("plan apply failed: %w; stdout=%s stderr=%s", applyErr, apply.stdout, apply.stderr)
	}
	if models.miniMaxCalls.Load() <= miniMaxBefore {
		return errors.New("allow phase never reached MiniMax")
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		return errors.New("source still exists after allowed quarantine")
	}
	listed, err := cli.mustRun("quarantine list", "quarantine", "list", "--json")
	if err != nil {
		return err
	}
	itemID, ok := findString(listed.stdout, "item_id")
	restorable, restorableOK := findBool(listed.stdout, "restorable")
	if !ok || !restorableOK || !restorable {
		return errors.New("allowed quarantine was not listed as restorable")
	}
	if _, err := cli.mustRun("quarantine restore", "quarantine", "restore", "--object", itemID, "--json"); err != nil {
		return err
	}
	restored, err := os.ReadFile(source)
	if err != nil || string(restored) != smokePayload {
		return errors.New("restored bytes differ from the quarantined bytes")
	}
	listed, err = cli.mustRun("quarantine list after restore", "quarantine", "list", "--json")
	if err != nil {
		return err
	}
	if bytes.Contains(listed.stdout, []byte(itemID)) {
		return errors.New("restored item is still listed in quarantine")
	}
	return nil
}

// mixedHistory is the one pass the two review phases cannot describe.
//
// Each phase above asks a single question of a single file, so nothing in them
// would notice a build where a deletion and a quarantine were counted as the
// same thing, where "the reviewers said no" was reported as an observation, or
// where one decision leaked onto every candidate in the batch. The mixed batch
// therefore puts three files that must be answered differently into one
// directory and runs the scheduled path once, the way the machine does:
//
//	removable.tmp   a temporary name, and the confirmed policy may delete it
//	                outright: the reviewers allow it, the file must be gone
//	observable.zip  an archive the policy only permits isolating: the
//	                reviewers allow the move, the file must be in the vault,
//	                restorable, with its bytes
//	declined.zip    the same archive class, and the reviewers refuse: the file
//	                must still be exactly where it was
//	recent.tmp      newer than the rule's age threshold, so it is not a
//	                candidate at all and must not appear on the page
//
// Everything is read back: the tree, the bytes, the vault and the page the user
// would be shown. The program's own summary is compared against them rather
// than believed on its own.
func runMixedBatch(artifact, dir, serverURL, caFile string, models *fakeModels) error {
	inst, err := newInstall(artifact, dir, serverURL, caFile)
	if err != nil {
		return err
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	fixtures := []struct {
		name    string
		payload string
		old     bool
	}{
		{"removable.tmp", "temporary payload\n", true},
		{"observable.zip", smokePayload, true},
		{"declined.zip", "declined payload\n", true},
		{"recent.tmp", "recent payload\n", false},
	}
	for _, f := range fixtures {
		path := filepath.Join(inst.tree, f.name)
		if err := os.WriteFile(path, []byte(f.payload), 0o600); err != nil {
			return err
		}
		if f.old {
			if err := os.Chtimes(path, old, old); err != nil {
				return err
			}
		}
	}
	// The reviewers refuse exactly one candidate. A single blanket answer would
	// let a batch look mixed while every file received the same treatment.
	models.keepPaths = map[string]bool{"declined.zip": true}
	defer func() { models.keepPaths = nil }()

	if _, err := inst.cli.mustRun("mixed scope add", "scope", "add",
		"--path", inst.tree, "--kind", "managed_disposable", "--name", "mixed", "--json"); err != nil {
		return err
	}
	if _, err := inst.cli.mustRun("mixed scan", "scan", "--scope", "mixed", "--json"); err != nil {
		return err
	}

	// The one-time confirmation, in its two documented calls. Only one category
	// is released for direct deletion, and that is what makes the first fixture's
	// outcome a deletion rather than an observation.
	policyArgs := []string{"cleanup", "setup", "--scopes", "mixed",
		"--categories", "temporary_name_only", "--observation-days", "7",
		"--notify", "none", "--json"}
	policyPreview, err := inst.cli.mustRun("mixed cleanup setup preview", policyArgs...)
	if err != nil {
		return err
	}
	policyDigest, ok := findString(policyPreview.stdout, "digest")
	if !ok {
		return errors.New("cleanup setup returned no digest")
	}
	if _, err := inst.cli.mustRun("mixed cleanup setup confirm",
		append(policyArgs, "--digest", policyDigest)...); err != nil {
		return err
	}

	ruleFile := filepath.Join(inst.dir, "mixed-rule.json")
	rule := `{"dsl_version":1,"scope_id":"mixed","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":1073741824,"relative_dir_prefixes":[],"basename_patterns":["*.tmp","*.zip"]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`
	if err := writePrivate(ruleFile, []byte(rule)); err != nil {
		return err
	}
	if _, err := inst.cli.mustRun("mixed rule import", "rule", "import",
		"--file", ruleFile, "--id", "mixed-rule", "--json"); err != nil {
		return err
	}
	// `rule enable --mode cleanup` is the standing authority for both action
	// classes: it activates the revision and records the grants the scheduled
	// path re-checks per item.
	enableArgs := []string{"rule", "enable", "--id", "mixed-rule", "--revision", "1", "--mode", "cleanup", "--json"}
	enablePreview, err := inst.cli.mustRun("mixed rule enable preview", enableArgs...)
	if err != nil {
		return err
	}
	enableDigest, ok := findString(enablePreview.stdout, "enable_digest")
	if !ok {
		return errors.New("rule enable returned no enable_digest")
	}
	if _, err := inst.cli.mustRun("mixed rule enable",
		append(enableArgs, "--digest", enableDigest)...); err != nil {
		return err
	}

	// The scheduled path, which is the only path that may delete a file without
	// an observation period, and the one the machine actually runs.
	runResult, runErr := inst.cli.mustRun("mixed run", "run", "--json")
	if runErr != nil {
		return runErr
	}

	// Witness 1: the tree. Exactly the two files that should still be there, with
	// the bytes they had. A directory listing is not a substitute for comparing
	// bytes, and comparing bytes is not a substitute for the listing. The
	// program's own bookkeeping directory lives beside them in a managed root and
	// is not one of the user's files, so the listing counts files only.
	entries, err := os.ReadDir(inst.tree)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "declined.zip" || names[1] != "recent.tmp" {
		return fmt.Errorf("after the mixed batch the tree holds %v, want [declined.zip recent.tmp]", names)
	}
	for name, want := range map[string]string{"declined.zip": "declined payload\n", "recent.tmp": "recent payload\n"} {
		body, err := os.ReadFile(filepath.Join(inst.tree, name))
		if err != nil || string(body) != want {
			return fmt.Errorf("%s is not the file that was there before the run (err=%v)", name, err)
		}
	}

	// Witness 2: the observation area. One object, restorable, and it is the
	// archive - not the file the reviewers refused, and not a second copy of the
	// one that was deleted.
	listed, err := inst.cli.mustRun("mixed quarantine list", "quarantine", "list", "--json")
	if err != nil {
		return err
	}
	if n := bytes.Count(listed.stdout, []byte(`"item_id"`)); n != 1 {
		return fmt.Errorf("the observation area holds %d item(s), want exactly 1: %s", n, listed.stdout)
	}
	itemID, ok := findString(listed.stdout, "item_id")
	if !ok {
		return errors.New("the observation area lists no object")
	}
	if restorable, ok := findBool(listed.stdout, "restorable"); !ok || !restorable {
		return errors.New("the observed object is not listed as restorable")
	}
	if _, err := inst.cli.mustRun("mixed quarantine restore",
		"quarantine", "restore", "--object", itemID, "--json"); err != nil {
		return err
	}
	restored, err := os.ReadFile(filepath.Join(inst.tree, "observable.zip"))
	if err != nil || string(restored) != smokePayload {
		return errors.New("the observed file did not come back with the bytes it had")
	}

	// Witness 3: the page the user is shown. Three outcomes, one each, and no
	// fourth row for the file the rule never matched.
	page, err := inst.cli.mustRun("mixed report", "cleanup", "report", "--json")
	if err != nil {
		return err
	}
	pageJSON, ok := findString(page.stdout, "page")
	if !ok {
		return errors.New("the mixed batch left no result page")
	}
	for _, want := range []struct {
		section string
		total   int
	}{{"deleted", 1}, {"to_be_deleted", 1}, {"kept", 1}} {
		got, ok := findSectionTotal([]byte(pageJSON), want.section)
		if !ok {
			return fmt.Errorf("the result page has no %q section: %s", want.section, pageJSON)
		}
		if got != want.total {
			return fmt.Errorf("the result page reports %d %q, want %d: %s", got, want.section, want.total, pageJSON)
		}
	}
	if bytes.Contains([]byte(pageJSON), []byte("recent.tmp")) {
		return fmt.Errorf("a file newer than the rule's age threshold appears on the page: %s", pageJSON)
	}
	// And the run's own summary has to agree with the page, because they are
	// written from different records and an operator reads whichever comes first.
	if purged, ok := findNumber(runResult.stdout, "purged"); !ok || purged != 1 {
		return fmt.Errorf("the run reported %d purge(s), want 1: %s", purged, runResult.stdout)
	}
	if quarantined, ok := findNumber(runResult.stdout, "quarantined"); !ok || quarantined != 1 {
		return fmt.Errorf("the run reported %d quarantine(s), want 1: %s", quarantined, runResult.stdout)
	}
	return nil
}

func writePrivate(path string, body []byte) error {
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

func findString(raw []byte, key string) (string, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return findStringValue(value, key)
}

func findBool(raw []byte, key string) (bool, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return findBoolValue(value, key)
}

func findBoolValue(value any, key string) (bool, bool) {
	switch v := value.(type) {
	case map[string]any:
		if b, ok := v[key].(bool); ok {
			return b, true
		}
		for _, child := range v {
			if b, ok := findBoolValue(child, key); ok {
				return b, true
			}
		}
	case []any:
		for _, child := range v {
			if b, ok := findBoolValue(child, key); ok {
				return b, true
			}
		}
	}
	return false, false
}

func findStringValue(value any, key string) (string, bool) {
	switch v := value.(type) {
	case map[string]any:
		if s, ok := v[key].(string); ok && s != "" {
			return s, true
		}
		for _, child := range v {
			if s, ok := findStringValue(child, key); ok {
				return s, true
			}
		}
	case []any:
		for _, child := range v {
			if s, ok := findStringValue(child, key); ok {
				return s, true
			}
		}
	}
	return "", false
}

// findNumber returns the first number stored under key, anywhere in raw.
func findNumber(raw []byte, key string) (int, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return findNumberValue(value, key)
}

func findNumberValue(value any, key string) (int, bool) {
	switch v := value.(type) {
	case map[string]any:
		if n, ok := v[key].(float64); ok {
			return int(n), true
		}
		for _, child := range v {
			if n, ok := findNumberValue(child, key); ok {
				return n, true
			}
		}
	case []any:
		for _, child := range v {
			if n, ok := findNumberValue(child, key); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// findSectionTotal returns the total of one named result-page section.
//
// The page groups its rows under the section names the product promises, and the
// count has to come from the section itself: a page that listed three rows under
// one heading would still contain the three file names the smoke looks for.
func findSectionTotal(raw []byte, section string) (int, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return sectionTotal(value, section)
}

func sectionTotal(value any, section string) (int, bool) {
	switch v := value.(type) {
	case map[string]any:
		if child, ok := v[section].(map[string]any); ok {
			if total, ok := child["total"].(float64); ok {
				return int(total), true
			}
		}
		for _, child := range v {
			if total, ok := sectionTotal(child, section); ok {
				return total, true
			}
		}
	case []any:
		for _, child := range v {
			if total, ok := sectionTotal(child, section); ok {
				return total, true
			}
		}
	}
	return 0, false
}

type fakeModels struct {
	allow        atomic.Bool
	miniMaxCalls atomic.Int64
	// keepPaths names the candidates the reviewers refuse, so one batch can
	// carry more than one answer. A nil map keeps the older behaviour, where
	// allow alone decided every candidate.
	keepPaths map[string]bool
}

func (m *fakeModels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/jev":
		if r.Header.Get("Authorization") != "Bearer "+jevKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if !m.allow.Load() || m.refuses(r) {
			_, _ = w.Write([]byte(`{"model":"jev-1.14.2","answers":{"quarantine_disposition":{"type":"choice","choice":"keep","confidence":0.99,"probabilities":{"allow_review":0.005,"keep":0.99,"insufficient":0.005}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.14.2","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.95,"probabilities":{"allow_review":0.95,"keep":0.03,"insufficient":0.02}}}}`))
	case "/minimax":
		m.miniMaxCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+miniMaxKey || r.Header.Get("x-api-key") != miniMaxKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_smoke","type":"message","role":"assistant","model":"MiniMax-Smoke","content":[{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`))
	default:
		http.NotFound(w, r)
	}
}

// refuses reports whether this request is about a candidate the fake reviewers
// decline. The facts the artifact sends carry the relative path, so the answer
// can differ per file while the endpoint stays one endpoint.
func (m *fakeModels) refuses(r *http.Request) bool {
	if len(m.keepPaths) == 0 || r.Body == nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return false
	}
	rel, ok := relativePathIn(body)
	if !ok {
		return false
	}
	return m.keepPaths[rel]
}

// relativePathIn finds the candidate's relative path in a review request.
//
// The measured facts travel as a JSON *string* inside the request envelope (the
// `state` field for JEV, the user message for MiniMax), so looking for the key
// in the envelope never matches it: every quote in it is escaped. Each string in
// the envelope is therefore read as a document of its own.
func relativePathIn(body []byte) (string, bool) {
	var envelope any
	if json.Unmarshal(body, &envelope) != nil {
		return "", false
	}
	return relativePathInValue(envelope)
}

func relativePathInValue(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		if !strings.Contains(v, `"relative_path"`) {
			return "", false
		}
		return findString([]byte(v), "relative_path")
	case map[string]any:
		for _, child := range v {
			if rel, ok := relativePathInValue(child); ok {
				return rel, true
			}
		}
	case []any:
		for _, child := range v {
			if rel, ok := relativePathInValue(child); ok {
				return rel, true
			}
		}
	}
	return "", false
}

func newTLSServer(handler http.Handler) (*httptest.Server, []byte, error) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Dustkeeper review smoke CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
	)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

func trustedClient(caPEM []byte) (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("append smoke CA")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}, nil
}
