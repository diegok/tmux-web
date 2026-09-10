package tmux

import (
	"context"
	"errors"
	"testing"
)

// The generation's two failure branches, which no test with a real tmux server
// can reach on cue: it is skipped when the snapshot itself failed, and a failure
// of its own leaves the last known generation in place.
//
// This is an internal test because both branches are decided inside refresh from
// functions only the constructors supply.
func TestRefreshKeepsTheLastKnownServerGeneration(t *testing.T) {
	var startCalls int
	var rowsErr, startErr error
	generation := "100"

	p := NewPollerFunc(0, func(context.Context) ([]Row, error) {
		if rowsErr != nil {
			return nil, rowsErr
		}
		return []Row{{PaneID: "%0"}}, nil
	})
	p.startFn = func(context.Context) (string, error) {
		startCalls++
		if startErr != nil {
			return "", startErr
		}
		return generation, nil
	}
	ctx := context.Background()

	p.refresh(ctx)
	if got := p.ServerStart(); got != "100" {
		t.Fatalf("ServerStart() = %q, want 100", got)
	}
	if startCalls != 1 {
		t.Fatalf("startFn called %d times in one poll, want 1", startCalls)
	}

	// A failed snapshot must not fork tmux a second time to ask a question the
	// same fault would fail, and must not disturb what is already known.
	rowsErr = errors.New("server exited unexpectedly")
	p.refresh(ctx)
	if startCalls != 1 {
		t.Errorf("startFn ran %d times; a failed poll must not ask for the generation", startCalls)
	}
	if got := p.ServerStart(); got != "100" {
		t.Errorf("a failed poll changed the generation to %q", got)
	}

	// A generation read that fails on its own keeps the previous value rather
	// than blanking it: every browser keys its per-pane memory on this, and an
	// empty generation for one poll makes all of it miss.
	rowsErr, startErr = nil, errors.New("nope")
	generation = "200" // must not be seen: this call errors
	p.refresh(ctx)
	if got := p.ServerStart(); got != "100" {
		t.Errorf("ServerStart() = %q after a failed read, want the last known 100", got)
	}

	// ... and a later success does replace it, or "keep the previous value"
	// would be indistinguishable from "never update".
	startErr = nil
	p.refresh(ctx)
	if got := p.ServerStart(); got != "200" {
		t.Errorf("ServerStart() = %q, want the new generation 200", got)
	}
}

// A poller built from a bare snapshot function has no tmux server to ask, and
// must report no generation rather than panicking on a nil reader.
func TestRefreshWithoutAServerGenerationReader(t *testing.T) {
	p := NewPollerFunc(0, func(context.Context) ([]Row, error) { return []Row{{PaneID: "%0"}}, nil })
	p.refresh(context.Background())
	if got := p.ServerStart(); got != "" {
		t.Fatalf("ServerStart() = %q, want empty", got)
	}
}
