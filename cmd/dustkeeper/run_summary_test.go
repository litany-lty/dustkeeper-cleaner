package main

import (
	"strings"
	"testing"

	"dustkeeper/internal/report"
	"dustkeeper/internal/run"
)

// A run that handled files but could not write its page must not answer with the
// three zeroed counters that stand in for "nothing happened": "0 deleted" is a
// claim about the files, and the run cannot make it without the record it failed
// to write.
func TestAFailedResultPageDoesNotReportZeroDeletions(t *testing.T) {
	rep := run.Report{RunID: "run_fixture", Quarantined: 2, Purged: 3, ReadBytes: 4096}

	lines := strings.Join(runSummaryLines(rep, report.Paths{}, [3]int{}, "mkdir /x: permission denied"), "\n")

	if strings.Contains(lines, "0 deleted") {
		t.Fatalf("a failed page reported zero deletions:\n%s", lines)
	}
	if !strings.Contains(lines, "breakdown is unknown") || !strings.Contains(lines, "permission denied") {
		t.Fatalf("the reason is missing:\n%s", lines)
	}
	// What the run does know still gets said.
	if !strings.Contains(lines, "purged 3 object(s)") {
		t.Fatalf("the run's own totals are missing:\n%s", lines)
	}
}

// The ordinary case must still carry all three outcomes and the page location.
func TestAWrittenPageReportsEveryOutcome(t *testing.T) {
	rep := run.Report{RunID: "run_fixture", Quarantined: 2, Purged: 3, ReadBytes: 4096}
	paths := report.Paths{Markdown: "/state/reports/latest.md"}

	lines := strings.Join(runSummaryLines(rep, paths, [3]int{1, 2, 3}, ""), "\n")

	for _, want := range []string{"1 deleted", "2 moved to the observation area", "3 left alone", paths.Markdown} {
		if !strings.Contains(lines, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "breakdown is unknown") {
		t.Errorf("a written page was described as unknown:\n%s", lines)
	}
}
