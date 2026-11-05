package domain

import "testing"

// B27: a read that uses exactly the configured allowance fits. File readers
// charge the final data chunk before they can observe EOF, so equality must not
// turn a fully budgeted verification into BUDGET_EXHAUSTED.
func TestB27ExactReadBudgetIsAllowed(t *testing.T) {
	ledger := NewRunLedger(60, 1024, 0)
	ledger.AddRead(1024)
	if !ledger.ReadUp() {
		t.Fatal("an exhausted allowance did not close the start-work gate")
	}
	if ledger.ReadExceeded() {
		t.Fatal("a read equal to the configured allowance was treated as over budget")
	}
	ledger.AddRead(1)
	if !ledger.ReadExceeded() {
		t.Fatal("a read beyond the configured allowance was not rejected")
	}
}
