package tmux

import (
	"context"
	"strings"
	"testing"
)

// stubSnapshotter counts calls so a test can assert that a refusal cost no fork.
type stubSnapshotter struct {
	rows  []Row
	calls int
}

func (s *stubSnapshotter) Snapshot(context.Context) ([]Row, error) {
	s.calls++
	return s.rows, nil
}

func TestCallerSessionPrefersTheSessionTheOwnerKeeps(t *testing.T) {
	// The app-owned row comes first, so a first-match lookup would get it wrong.
	s := &stubSnapshotter{rows: []Row{
		{PaneID: "%5", SessionID: "$7", AppOwned: true},
		{PaneID: "%5", SessionID: "$0"},
	}}
	got, err := CallerSession(context.Background(), s, "%5")
	if err != nil || got != "$0" {
		t.Fatalf("CallerSession = %q, %v; want $0", got, err)
	}
}

func TestCallerSessionAnswersWithOnlyAnAppSession(t *testing.T) {
	s := &stubSnapshotter{rows: []Row{{PaneID: "%5", SessionID: "$7", AppOwned: true}}}
	got, err := CallerSession(context.Background(), s, "%5")
	if err != nil || got != "$7" {
		t.Fatalf("CallerSession = %q, %v; want $7 and no error", got, err)
	}
}

func TestCallerSessionSaysSoForAPaneThatIsNotThere(t *testing.T) {
	s := &stubSnapshotter{rows: []Row{{PaneID: "%6", SessionID: "$0"}}}
	_, err := CallerSession(context.Background(), s, "%5")
	if err == nil || !strings.Contains(err.Error(), "%5") {
		t.Fatalf("err = %v; want one naming %%5", err)
	}
}

func TestCallerSessionRefusesAMalformedPaneID(t *testing.T) {
	s := &stubSnapshotter{}
	if _, err := CallerSession(context.Background(), s, "5"); err == nil {
		t.Fatal("want an error for \"5\"")
	}
	if s.calls != 0 {
		t.Fatalf("Snapshot called %d times; a bad id must cost no fork", s.calls)
	}
}
