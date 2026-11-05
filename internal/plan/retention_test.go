package plan

import (
	"testing"
	"time"
)

// TestEffectiveRetention pins the one rule that decides how long a newly
// isolated object is kept: the longer of what the rule promised and what the
// cleanup policy promised.
//
// The direction matters more than the arithmetic. Every wrong answer that
// shortens the period is a promise broken to a user who was told a date, so
// neither input may ever win by being smaller.
func TestEffectiveRetention(t *testing.T) {
	const day = int64(24 * 60 * 60)
	cases := []struct {
		name       string
		ruleSecond int64
		policySec  int64
		want       int64
	}{
		{"a seven day rule under the fourteen day default is extended", 7 * day, 14 * day, 14 * day},
		{"a thirty day rule is never shortened to the default", 30 * day, 14 * day, 30 * day},
		{"a thirty day rule equals a thirty day policy", 30 * day, 30 * day, 30 * day},
		{"the policy is irrelevant when it is unset", 30 * day, 0, 30 * day},
		{"a rule with no retention still gets the policy period", 0, 14 * day, 14 * day},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveRetention(tc.ruleSecond, tc.policySec); got != tc.want {
				t.Fatalf("effectiveRetention(%d, %d) = %d, want %d",
					tc.ruleSecond, tc.policySec, got, tc.want)
			}
		})
	}
}

// TestEffectiveRetentionNeverShortensTheRule is the property form of the same
// promise, checked over a range rather than at chosen points: whatever the
// policy says, the answer is at least what the rule said.
func TestEffectiveRetentionNeverShortensTheRule(t *testing.T) {
	days := []int64{0, 1, 3, 7, 14, 15, 30, 90, 365}
	for _, rule := range days {
		for _, policy := range days {
			got := effectiveRetention(rule, policy)
			if got < rule {
				t.Fatalf("rule=%d days policy=%d days produced %s, which is shorter than the rule",
					rule, policy, time.Duration(got)*time.Second)
			}
			if got < policy {
				t.Fatalf("rule=%d days policy=%d days produced %s, which is shorter than the policy",
					rule, policy, time.Duration(got)*time.Second)
			}
		}
	}
}
