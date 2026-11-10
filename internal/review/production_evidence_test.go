package review

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestProductionMiniMaxRequestsAndReturnsStructuredMissingEvidence(t *testing.T) {
	j := generalJudge(t, allowJEVBody,
		`{"decision":"insufficient","reason_code":"insufficient_evidence","missing_evidence":["current_use","references"]}`)
	original := j.client.Transport
	j.client = &http.Client{Transport: generalTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/minimax" {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			var request struct {
				System string `json:"system"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"missing_evidence", "purpose_or_disposal_basis", "references", "current_use", "recoverability", "sensitive_content", "provenance", "action_constraints"} {
				if !strings.Contains(request.System, want) {
					t.Errorf("production prompt does not request %q: %s", want, request.System)
				}
			}
			if strings.Contains(request.System, "explanation") {
				t.Errorf("production prompt requested free-text explanation: %s", request.System)
			}
		}
		return original.RoundTrip(r)
	})}

	d, err := j.Review(context.Background(), testCandidate())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"current_use", "references"}; !reflect.DeepEqual(d.MissingEvidence, want) {
		t.Fatalf("missing evidence = %#v, want %#v", d.MissingEvidence, want)
	}
}

func TestProductionMiniMaxKeepsLegacyMissingEvidenceAbsent(t *testing.T) {
	j := generalJudge(t, allowJEVBody, `{"decision":"allow","reason_code":"disposable"}`)
	d, err := j.Review(context.Background(), testCandidate())
	if err != nil {
		t.Fatal(err)
	}
	if d.MissingEvidence != nil {
		t.Fatalf("legacy response invented missing evidence: %#v", d.MissingEvidence)
	}
}

func TestProductionMiniMaxRejectsInvalidMissingEvidence(t *testing.T) {
	for name, decision := range map[string]string{
		"unknown category": `{"decision":"insufficient","reason_code":"insufficient_evidence","missing_evidence":["unknown_fact"]}`,
		"too many values":  `{"decision":"insufficient","reason_code":"insufficient_evidence","missing_evidence":["current_use","current_use","current_use","current_use","current_use","current_use","current_use","current_use","current_use"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			j := generalJudge(t, allowJEVBody, decision)
			d, err := j.Review(context.Background(), testCandidate())
			if err == nil || d.Verdict == "allow" {
				t.Fatalf("invalid missing evidence accepted: decision=%+v err=%v", d, err)
			}
		})
	}
}
