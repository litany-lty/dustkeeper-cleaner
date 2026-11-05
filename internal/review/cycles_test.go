package review

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCycle13ProviderTrustDoesNotCrossBoundaries(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certA, keyA := cycleCA(t, "jev-only")
	certB, _ := cycleCA(t, "minimax-only")
	pathA := filepath.Join(dir, "a.pem")
	pathB := filepath.Join(dir, "b.pem")
	os.WriteFile(pathA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certA.Raw}), 0600)
	os.WriteFile(pathB, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certB.Raw}), 0600)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(3), DNSNames: []string{"minimax.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, certA, &leafKey.PublicKey, keyA)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	j, err := NewHTTPJudge(Endpoint{URL: "https://jev.test", Model: "jev-test", APIKey: "test", ProxyURL: "http://127.0.0.1:1", TrustCAFile: pathA}, Endpoint{URL: "https://minimax.test", Model: "mm", APIKey: "test", TrustCAFile: pathB})
	if err != nil {
		t.Fatal(err)
	}
	roots := j.client.Transport.(*http.Transport).TLSClientConfig.RootCAs
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "minimax.test", Roots: roots}); err == nil {
		t.Fatal("JEV-only CA trusted for MiniMax")
	}
}
func cycleCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}
func TestCycle14UsageCannotInventZeroOrIgnoreCaseCollisions(t *testing.T) {
	for _, usage := range []string{`{}`, `{"input_tokens":2}`, `{"input_tokens":2,"INPUT_TOKENS":9,"output_tokens":1}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"model":"jev-test","answers":{"quarantine_disposition":{"type":"choice","choice":"keep","confidence":0.99,"probabilities":{"allow_review":0.005,"keep":0.99,"insufficient":0.005}}},"usage":%s}`, usage)
		}))
		j, err := NewHTTPJudge(Endpoint{URL: srv.URL, Model: "jev-test", APIKey: "x"}, Endpoint{URL: srv.URL, Model: "mm", APIKey: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := j.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"}); err == nil {
			t.Errorf("invalid token usage accepted: %s", usage)
		}
		srv.Close()
	}
}
func TestCycle16PrivatePermissionsIncludeNativeACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS ACL fixture")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "secret.json")
	os.WriteFile(p, []byte(`{"secret":"fixture"}`), 0600)
	if err := exec.Command("/bin/chmod", "+a", "everyone allow read", p).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("/bin/chmod", "-N", p).Run() })
	if _, err := ReadPrivateFile(p, 1024); err == nil {
		t.Fatal("world-readable ACL accepted as private")
	}
}
func TestCycle27InputLimitIncludesInstructionsAndEnvelope(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"model":"jev-test","answers":{"quarantine_disposition":{"type":"choice","choice":"keep","confidence":0.99,"probabilities":{"allow_review":0.005,"keep":0.99,"insufficient":0.005}}}}`)
	}))
	defer srv.Close()
	j, err := NewHTTPJudge(Endpoint{URL: srv.URL, Model: "jev-test", APIKey: "fixture"}, Endpoint{URL: srv.URL, Model: "mm", APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	// The limit covers the facts document, not the file name alone: a single
	// very long relative path has to be refused before anything is sent.
	c := Facts{Category: "temporary_name_only", Action: "quarantine",
		RelativePath: strings.Repeat("a", maxJudgeInput)}
	if _, err := j.Review(context.Background(), c); err == nil || calls.Load() != 0 {
		t.Fatalf("oversize request sent: calls=%d err=%v", calls.Load(), err)
	}
}
func TestCycle30MiniMaxUsageSurvivesTheDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/j" {
			fmt.Fprint(w, `{"model":"jev-test","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.99,"probabilities":{"allow_review":0.99,"keep":0.005,"insufficient":0.005}}}}`)
			return
		}
		fmt.Fprint(w, `{"id":"msg-fixture","type":"message","role":"assistant","model":"mm","content":[{"type":"text","text":"{\"decision\":\"deny\",\"reason_code\":\"still_needed\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":321,"output_tokens":45,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}`)
	}))
	defer srv.Close()
	j, err := NewHTTPJudge(Endpoint{URL: srv.URL + "/j", Model: "jev-test", APIKey: "fixture"}, Endpoint{URL: srv.URL + "/m", Model: "mm", APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := j.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	var out map[string]any
	json.Unmarshal(b, &out)
	usage, ok := out["llm_usage"].(map[string]any)
	if !ok || usage["input_tokens"] != float64(321) || usage["output_tokens"] != float64(45) {
		t.Fatalf("lost billed model usage: %s", b)
	}
}
