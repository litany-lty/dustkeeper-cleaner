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
	fmt.Println("review smoke: keep retained the source; allowed quarantine restored identical bytes")
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

func runPhase(artifact, dir, serverURL, caFile string, models *fakeModels, allow bool) error {
	for _, sub := range []string{"tree", "state", "config", "home"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	tree := filepath.Join(dir, "tree")
	source := filepath.Join(tree, "old.zip")
	if err := os.WriteFile(source, []byte(smokePayload), 0o600); err != nil {
		return err
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(source, old, old); err != nil {
		return err
	}
	// No evidence file and no retained copy: the release smoke exercises the
	// ordinary path, where the program measures the facts itself.
	credentials := filepath.Join(dir, "credentials.json")
	if err := writePrivate(credentials, []byte(`{"typesafe_api_key":"`+jevKey+`","minimax_api_key":"`+miniMaxKey+`"}`)); err != nil {
		return err
	}

	cli := commandEnv{artifact: artifact, state: filepath.Join(dir, "state"), config: filepath.Join(dir, "config"), home: filepath.Join(dir, "home")}
	if _, err := cli.mustRun("init", "init", "--json"); err != nil {
		return err
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
		return err
	}
	if err := writePrivate(filepath.Join(cli.config, "config.json"), configJSON); err != nil {
		return err
	}
	ruleFile := filepath.Join(dir, "rule.json")
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

type fakeModels struct {
	allow        atomic.Bool
	miniMaxCalls atomic.Int64
}

func (m *fakeModels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/jev":
		if r.Header.Get("Authorization") != "Bearer "+jevKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if !m.allow.Load() {
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
