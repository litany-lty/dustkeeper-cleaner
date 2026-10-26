package rules

import (
	"encoding/json"
	"testing"
)

// vectorJSON is the canonical form published in section 6.6. It is one line, no
// BOM and no trailing newline, because those details are part of the digest.
const vectorJSON = `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":2592000,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":["*.zip"]},"except":{"relative_dir_prefixes":["keep/"],"basename_patterns":["*发票*"]},"retention_seconds":2592000}`

const vectorDigest = "sha256:a1128b67a0e576202f6023f8b21bf69565f720910b8d23799b7c64527a76f4d6"

// TestDigestTestVector pins field order, escaping, array order and prefix rules
// against the published vector. If this fails, every stored approval is suspect.
func TestDigestTestVector(t *testing.T) {
	allowed := map[string]bool{"exports": true}
	r, err := Parse([]byte(vectorJSON), allowed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	canon, err := r.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(canon) != vectorJSON {
		t.Fatalf("canonical form differs\n got: %s\nwant: %s", canon, vectorJSON)
	}
	d, err := r.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if string(d) != vectorDigest {
		t.Fatalf("digest = %s, want %s", d, vectorDigest)
	}
}

// TestDigestStableAcrossKeyOrder proves the digest does not depend on the order
// the user wrote the keys in.
func TestDigestStableAcrossKeyOrder(t *testing.T) {
	reordered := `{"retention_seconds":2592000,"except":{"basename_patterns":["*发票*"],"relative_dir_prefixes":["keep/"]},"match":{"basename_patterns":["*.zip"],"relative_dir_prefixes":[],"max_size_bytes":null,"min_size_bytes":0,"min_mtime_age_seconds":2592000},"action":"quarantine","scope_id":"exports","dsl_version":1}`
	allowed := map[string]bool{"exports": true}
	r, err := Parse([]byte(reordered), allowed)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d, _ := r.Digest()
	if string(d) != vectorDigest {
		t.Fatalf("digest depends on key order: %s", d)
	}
}

// TestRejectsMalformedInput covers the strict-token contract.
func TestRejectsMalformedInput(t *testing.T) {
	allowed := map[string]bool{"exports": true}
	cases := map[string]string{
		"duplicate key":      `{"dsl_version":1,"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"unknown field":      `{"dsl_version":1,"scope_id":"exports","action":"quarantine","delete":true,"match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"float age":          `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400.5,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"exponent":           `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":8.64e4,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"string number":      `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":"86400","min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"trailing data":      `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800} {"x":1}`,
		"delete action":      `{"dsl_version":1,"scope_id":"exports","action":"delete","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"unregistered scope": `{"dsl_version":1,"scope_id":"elsewhere","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"absolute prefix":    `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":["/etc/"],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"dotdot prefix":      `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":["../"],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"prefix no slash":    `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":["keep"],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"double star":        `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":["**/*.zip"]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"character class":    `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":["[ab].zip"]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"short retention":    `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":3600}`,
		"partial day":        `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604801}`,
		"age too small":      `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":60,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"max below min":      `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":100,"max_size_bytes":10,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
		"duplicate pattern":  `{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"max_size_bytes":null,"relative_dir_prefixes":[],"basename_patterns":["*.zip","*.zip"]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`,
	}
	for name, body := range cases {
		if _, err := Parse([]byte(body), allowed); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// TestExplainMentionsEveryEffectiveField keeps the approval text tied to the
// structure rather than to model prose.
func TestExplainMentionsEveryEffectiveField(t *testing.T) {
	r, err := Parse([]byte(vectorJSON), map[string]bool{"exports": true})
	if err != nil {
		t.Fatal(err)
	}
	lines := r.Explain()
	if len(lines) < 4 {
		t.Fatalf("explanation too short: %v", lines)
	}
	joined, _ := json.Marshal(lines)
	for _, want := range []string{"scope", "match", "exclude", "retention", "grant"} {
		if !containsSub(string(joined), want) {
			t.Errorf("explanation missing %q: %s", want, joined)
		}
	}
}

func containsSub(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
