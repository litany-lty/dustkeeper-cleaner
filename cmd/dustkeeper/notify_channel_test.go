package main

import (
	"context"
	"io"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/notify"
)

// The confirmed policy is where a user says whether they want to be told. That
// answer used to reach the reminder in front of a clearing and nothing else: the
// sender itself stayed the macOS one, so a server running with `--notify none`
// still ran osascript after every action, and reported the failure it could not
// possibly avoid as a fault of the run.
//
// These two tests are the pair that makes the choice mean something: the same
// seam is used in both, and only the policy differs.

// TestAConfirmedChoiceOfNoNotificationsNeverReachesASender is the defect.
func TestAConfirmedChoiceOfNoNotificationsNeverReachesASender(t *testing.T) {
	_, _ = cleanupFixture(t)

	// Confirm it the way a person does: preview, read the digest, confirm that
	// digest with the same flags.
	preview := mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports", "--notify", "none")
	digest, _ := dataOf(t, preview)["digest"].(string)
	if digest == "" {
		t.Fatalf("the preview carries no digest: %v", preview.stdout)
	}
	mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports", "--notify", "none", "--digest", digest)

	// A delivering channel is installed on the seam the command line uses, so a
	// send would be observed rather than assumed absent.
	sender := &cliSender{}
	testSender = sender
	t.Cleanup(func() { testSender = nil })

	a := appForNotificationTest(t)
	if _, ok := a.notifySender().(notify.None); !ok {
		t.Fatalf("a confirmed policy of no notifications still selects %T", a.notifySender())
	}
	res := a.notifySender().Send(context.Background(), "Dustkeeper", "本轮没有文件被删除或移动。")
	if res.Attempted || res.Delivered {
		t.Fatalf("the channel reports an attempt although the user turned notifications off: %+v", res)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("the sender was called %d time(s) although the user turned notifications off: %q", len(sender.sent), sender.sent)
	}
}

// TestTheSameSeamDeliversWhenThePolicyAsksForIt is the control.
//
// Without it, the test above would pass on an installation where nothing is
// wired at all. The only difference between the two is the confirmed choice.
func TestTheSameSeamDeliversWhenThePolicyAsksForIt(t *testing.T) {
	_, _ = cleanupFixture(t)

	sender := &cliSender{}
	testSender = sender
	t.Cleanup(func() { testSender = nil })

	a := appForNotificationTest(t)
	if _, ok := a.notifySender().(notify.None); ok {
		t.Fatal("the default policy selects the channel that never tries")
	}
	if res := a.notifySender().Send(context.Background(), "Dustkeeper", "one file was moved"); !res.Delivered {
		t.Fatalf("the delivering channel did not deliver: %+v", res)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("the sender was called %d time(s), want 1", len(sender.sent))
	}
}

// appForNotificationTest opens the instance the way a command does, because the
// channel is chosen from the confirmed policy stored in it.
func appForNotificationTest(t *testing.T) *app {
	t.Helper()
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	a := &app{
		paths:  paths,
		clock:  domain.NewFakeClock(time.Now()),
		stdout: io.Discard,
		stderr: io.Discard,
	}
	if err := a.openStore(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if a.store != nil {
			a.store.Close()
		}
	})
	return a
}
