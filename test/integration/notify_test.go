package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/notify"
)

// R27 in one sentence: a line in a log is not a notification.
//
// The clearing step happens while nobody is watching, so the only thing that may
// let it proceed is a delivery the channel reported as successful. These tests
// pin both halves: the reminder that gates the clearing, and the summary that
// tells the user what happened.
func TestUndeliveredReminderStopsTheClearing(t *testing.T) {
	e, sc, it := dueObjectAcrossSevenDays(t)
	// An unattended machine: no GUI session, so the system notification fails.
	e.sender.fail = errors.New("no GUI session is available")

	rep := e.step(47 * time.Hour)
	if rep.Purged != 0 {
		t.Fatalf("the run cleared an object the user was never told about: %+v", rep)
	}
	if len(rep.Scopes) != 1 || !strings.Contains(rep.Scopes[0].PurgeSkipped, "notify_failed") {
		t.Fatalf("the run does not say why nothing was cleared: %+v", rep.Scopes)
	}
	if _, err := os.Stat(e.vaultPath("exports", it.ObjectName)); err != nil {
		t.Fatalf("the object was removed anyway: %v", err)
	}
	// It is still due, not counted as refused: the next run has to be able to
	// clear it once the environment is fixed.
	if items := e.quarantinedItems(sc.ID); len(items) != 1 {
		t.Fatalf("%d object(s) remain in the observation area", len(items))
	}

	// The failed attempt is on the record. A gate whose failures are invisible is
	// indistinguishable from a gate that never ran.
	attempts := notifyEvents(t, e)
	if len(attempts) == 0 {
		t.Fatal("the failed reminder was not recorded")
	}
	last := attempts[len(attempts)-1]
	if last.Delivered || last.Channel != notify.ChannelMacOS || last.Detail == "" {
		t.Fatalf("the recorded attempt does not describe a failure: %+v", last)
	}

	// Once the channel works again, the same object clears on the next run.
	e.sender.fail = nil
	if rep := e.step(47 * time.Hour); rep.Purged != 1 {
		t.Fatalf("the object was not cleared after the channel recovered: %+v", rep)
	}
}

// TestQuarantineIsAnnouncedAndRecorded is the other half of R27: an object moved
// into the observation area is something the user is told about, once, with the
// date it can be cleared.
func TestQuarantineIsAnnouncedAndRecorded(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	rep := e.runOnce()
	if rep.Quarantined != 1 {
		t.Fatalf("expected one object to be observed: %+v", rep)
	}
	messages := e.sender.messages()
	if len(messages) != 1 {
		t.Fatalf("expected one message, got %v", messages)
	}
	if !strings.Contains(messages[0], "1 个文件进入待删除区") || !strings.Contains(messages[0], "14 天") {
		t.Fatalf("the message does not say what happened or when it clears: %q", messages[0])
	}
	if len(rep.Scopes) != 1 || rep.Scopes[0].Notification == nil || !rep.Scopes[0].Notification.Delivered {
		t.Fatalf("the report does not carry the delivery: %+v", rep.Scopes)
	}
	recorded := notifyEvents(t, e)
	if len(recorded) != 1 || !recorded[0].Delivered || recorded[0].Summary == "" {
		t.Fatalf("the delivery was not recorded: %+v", recorded)
	}
}

// TestNothingTouchedMeansNothingSent keeps the channel meaningful: a message that
// says "0 deleted, 0 moved" every morning is how a user learns to ignore the one
// that matters.
func TestNothingTouchedMeansNothingSent(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "keep.txt", "not a candidate", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	rep := e.runOnce()
	if rep.Quarantined != 0 || rep.Purged != 0 {
		t.Fatalf("the fixture touched a file: %+v", rep)
	}
	if got := e.sender.messages(); len(got) != 0 {
		t.Fatalf("a run that did nothing sent %v", got)
	}
}

// delivery is one recorded notification outcome.
type delivery struct {
	notify.Result
	Summary string `json:"summary"`
}

// notifyEvents returns the recorded delivery outcomes, oldest first.
func notifyEvents(t *testing.T, e *env) []delivery {
	t.Helper()
	events, err := e.st.ListEvents(context.Background(), 500, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var out []delivery
	for _, ev := range events {
		if ev.Action != notify.EventAction || ev.ResultJSON == nil {
			continue
		}
		var payload delivery
		if err := json.Unmarshal([]byte(*ev.ResultJSON), &payload); err != nil {
			t.Fatalf("notify payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}
