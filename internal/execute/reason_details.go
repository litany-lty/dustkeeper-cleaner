package execute

import (
	"dustkeeper/internal/review"
	"encoding/json"
)

// Add the same payload-free explanation to the authoritative settlement event.
// Historical events lacking a summary remain unchanged.
func withReviewExplanation(result *string, explanation *review.Explanation) *string {
	if result == nil || explanation == nil {
		return result
	}
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(*result), &body) != nil || body == nil {
		return result
	}
	raw, err := json.Marshal(explanation)
	if err != nil {
		return result
	}
	body["review_explanation"] = raw
	encoded, err := json.Marshal(body)
	if err != nil {
		return result
	}
	s := string(encoded)
	return &s
}
