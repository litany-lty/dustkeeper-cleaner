package review

import (
	"context"
	"dustkeeper/internal/domain"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewTLSStillRequiresTrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"jev-test","answers":{"quarantine_disposition":{"type":"choice","choice":"keep","confidence":0.99,"probabilities":{"allow_review":0.005,"keep":0.99,"insufficient":0.005}}}}`))
	}))
	defer srv.Close()
	ep := Endpoint{URL: srv.URL, Model: "jev-test", APIKey: "fixture"}
	j, e := NewHTTPJudge(ep, ep)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = j.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"}); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "ca.pem")
	if e = os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	ep.TrustCAFile = p
	j, e = NewHTTPJudge(ep, ep)
	if e != nil {
		t.Fatal(e)
	}
	d, e := j.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"})
	if e != nil || d.Verdict == domain.VerdictAllow {
		t.Fatalf("trusted keep response failed: %+v %v", d, e)
	}
}
