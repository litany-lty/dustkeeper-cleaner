package main

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
)

func quarantineOneWithReview(t *testing.T, clk domain.Clock) (tree, itemID, objectName string) {
	t.Helper()
	base, tree := scopeWithRule(t, clk)
	installClockReviewFixture(t, base, tree, clk)
	created := mustCLIOn(t, clk, "plan", "create", "--rule", "r1", "--json")
	mustCLIOn(t, clk, "plan", "apply", "--id", dataString(t, created, "plan_id"), "--digest", dataString(t, created, "digest"), "--json")
	listed := mustCLIOn(t, clk, "quarantine", "list", "--json")
	objects, _ := dataOf(t, listed)["objects"].([]any)
	if len(objects) != 1 {
		t.Fatalf("the vault holds %d object(s), want one", len(objects))
	}
	row, _ := objects[0].(map[string]any)
	itemID, _ = row["item_id"].(string)
	objectName, _ = row["object_name"].(string)
	if itemID == "" || objectName == "" {
		t.Fatalf("the list row does not identify the object: %v", row)
	}
	return tree, itemID, objectName
}

// installClockReviewFixture uses production configuration and review.Service;
// only the two HTTPS providers are local synthetic data. There is no per-file
// evidence file any more: the facts the reviewers see are measured by the
// program at the moment of the decision.
func installClockReviewFixture(t *testing.T, base, tree string, clk domain.Clock, _ ...string) {
	t.Helper()
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	privateDir := filepath.Join(resolvedBase, "review-fixture")
	if err := os.MkdirAll(privateDir, 0700); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/jev" {
			_, _ = w.Write([]byte(`{"model":"jev-1.14.0","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.99,"probabilities":{"allow_review":0.99,"keep":0.005,"insufficient":0.005}}},"usage":{"input_tokens":20,"output_tokens":4}}`))
			return
		}
		var request struct {
			System string `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		decision := `{"decision":"allow","reason_code":"disposable"}`
		if strings.Contains(request.System, "For debug only") {
			decision = `{"decision":"allow","reason_code":"disposable","explanation":"Synthetic evidence fixture.","missing_evidence":[]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg-clock-fixture", "type": "message", "role": "assistant", "model": "MiniMax-M3.1-Flash", "content": []map[string]string{{"type": "text", "text": decision}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 30, "output_tokens": 5}})
	}))
	t.Cleanup(server.Close)
	caPath := filepath.Join(privateDir, "review-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(privateDir, "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"typesafe_api_key":"fixture-jev","minimax_api_key":"fixture-minimax"}`), 0600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(base, "config", "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["review"] = map[string]any{
		"trust_ca_file": caPath, "credentials_file": credentialsPath,
		"jev_url": server.URL + "/jev", "jev_model": "jev-1.14.0",
		"minimax_url": server.URL + "/minimax", "minimax_model": "MiniMax-M3.1-Flash",
		"max_candidates": 10,
	}
	raw, _ = json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
