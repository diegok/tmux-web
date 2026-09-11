package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// Writer to tmux to reader, with nothing stubbed between them: the real
// subcommand writes the real option on a real tmux server, and the real
// batched read puts it on a real Row.
//
// Against testutil's own server, never the developer's. `report` in production
// addresses whatever $TMUX names, so a test that let $TMUX leak through from
// the environment it runs in would write into the developer's live session --
// which holds real work and running agents.
//
// TWO panes, deliberately, and the second one is load-bearing twice over.
//
//   - `set` without `-p` writes a SESSION option, and tmux resolves
//     #{@tmux_web_agent} up the hierarchy -- pane, then window, then session, then
//     global -- so a session-level value shows through the PANE format on every
//     pane of that session. Measured on an isolated socket: after
//     `set -t work @tmux_web_agent SESSIONLEVEL`,
//     `list-panes -a -F '#{@tmux_web_agent}'` printed SESSIONLEVEL for both panes.
//     A one-pane fixture therefore cannot see the missing `-p` at all.
//   - `set -p` without `-t` writes to the CURRENT pane, which with no client
//     attached is the session's active one. Measured on the same socket: the
//     value landed on %0, the pane `split-window -d` left active. So the pane
//     this test reports on is deliberately the INACTIVE one -- otherwise the
//     dropped-target mutant writes exactly where the assertion is looking and
//     survives.
//
// And the assertion is on the VALUE, never on the presence of the key. Task 3's
// batched read gives every pane a line, empty value and all, so
// `if _, ok := reports[paneID]; !ok` is a check on something Task 3 guarantees
// unconditionally -- it holds for a pane that was never written to, and it held
// for the missing-`-p` mutant too.
func TestReportReachesTheSnapshot(t *testing.T) {
	srv, paneID, otherPane := twoAgentPanes(t)

	var out, errb bytes.Buffer
	// dialReal, because this test is the end-to-end one: a real client against
	// the real server testutil started.
	if code := runReport([]string{"--state", "working", "--text", "running go test"},
		strings.NewReader(""), &out, &errb, paneEnv(srv, paneID), dialReal); code != 0 {
		t.Fatalf("report exited %d: %s", code, errb.String())
	}

	c := tmux.NewClient(srv.Args())
	rows, reports, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	// The value, parsed. Not the key.
	rep, ok := tmux.ParseReport(reports[paneID], time.Now())
	if !ok || rep.State != tmux.StateWorking || rep.Activity != "running go test" {
		t.Fatalf("report for %s = %q -> %+v, %v; want a working report reading %q",
			paneID, reports[paneID], rep, ok, "running go test")
	}
	// The pane that was NOT written to must read empty. This is the half that
	// kills the option-scope mutants: a session option shows through the pane
	// format on every pane of the session, and a write with no target lands on
	// the active pane, which is this one.
	if reports[otherPane] != "" {
		t.Fatalf("the pane nobody reported on reads %q: the write was not scoped to this pane (`set` without `-p` sets a SESSION option, which tmux resolves through #{@tmux_web_agent} on every pane of the session; `set -p` without `-t` lands on the active pane)",
			reports[otherPane])
	}
	if len(rows) != 2 {
		t.Fatalf("snapshot has %d rows, want the 2 panes of the fixture", len(rows))
	}

	// And through the poller's precedence, which is what the sidebar sees.
	//
	// Connected is false: no browser, no capture, no classifier -- so the row
	// below is carried entirely by the report, which is the case the whole
	// feature exists for. (Capture is still wired, because NewPollerWith
	// refuses half-wired classification; with nobody connected it is never
	// called.)
	p := tmux.NewPollerWith(tmux.Options{
		Interval:            time.Hour, // Start polls once synchronously; nothing here needs a second poll
		SnapshotWithReports: c.SnapshotAndReports,
		ServerStart:         c.ServerStart,
		Capture:             c.Capture,
		Connected:           func() bool { return false },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	row := rowFor(t, p.Latest(), paneID)
	if row.AgentState != tmux.StateWorking || row.StateSource != tmux.SourceEvent || row.Activity != "running go test" {
		t.Errorf("the reporting pane's row = {state:%q source:%q activity:%q}, want {working event %q} with no client connected",
			row.AgentState, row.StateSource, row.Activity, "running go test")
	}
	other := rowFor(t, p.Latest(), otherPane)
	if other.AgentState != "" || other.StateSource != "" || other.Activity != "" {
		t.Errorf("the pane nobody reported on carries agent state {state:%q source:%q activity:%q}, want none",
			other.AgentState, other.StateSource, other.Activity)
	}
}

// A state-only report survives the round trip through tmux as three fields.
//
// This is the trailing-";" trap, at the one place it actually bites: tmux's own
// command parser eats a trailing separator, and a value of exactly ";" is
// refused with "empty value" while the option KEEPS its previous contents --
// the more dangerous of the two outcomes, since a botched write would then look
// like a standing report. FormatReport never emits that structural separator;
// this is what proves the claim against a real server rather than against
// Go's idea of one.
func TestAStateOnlyReportIsThreeFields(t *testing.T) {
	srv, paneID, _ := twoAgentPanes(t)

	var out, errb bytes.Buffer
	if code := runReport([]string{"--state", "idle"}, strings.NewReader(""), &out, &errb, paneEnv(srv, paneID), dialReal); code != 0 {
		t.Fatalf("report exited %d: %s", code, errb.String())
	}

	stored := srv.Run(t, "show", "-p", "-t", paneID, "-v", tmux.AgentOption)
	if !regexp.MustCompile(`^1;idle;[0-9]{13}$`).MatchString(stored) {
		t.Fatalf("@tmux_web_agent = %q, want 1;idle;<13 digits> and no trailing separator", stored)
	}
	if rep, ok := tmux.ParseReport(stored, time.Now()); !ok || rep.State != tmux.StateIdle || rep.Activity != "" {
		t.Fatalf("ParseReport(%q) = %+v, %v; want a state-only idle report", stored, rep, ok)
	}
}

// A state this daemon does not know writes NOTHING. Exit 0 is not the
// assertion -- every path here exits 0 -- so the assertion is on the pane
// option.
//
// The stderr line is asserted too, and that is not decoration. The plan's
// mutation table expects "the option must be unset" to kill a dropped state
// switch; measured, it does not. FormatReport would happily build
// `1;nonsense;<ts>`, and the writer's own read-back check refuses it before the
// write, so the option stays unset either way and the mutant survives on
// behaviour alone. What the switch is actually worth is the diagnostic: the
// author of an integration that sent a state we do not know is told which state
// it was, instead of the read-back check's "could not read back", which names
// nothing and points at the value rather than at the caller.
func TestAnUnknownStateWritesNothing(t *testing.T) {
	srv, paneID, _ := twoAgentPanes(t)

	var out, errb bytes.Buffer
	if code := runReport([]string{"--state", "nonsense", "--text", "something"}, strings.NewReader(""),
		&out, &errb, paneEnv(srv, paneID), dialReal); code != 0 {
		t.Fatalf("report exited %d: %s", code, errb.String())
	}

	c := tmux.NewClient(srv.Args())
	_, reports, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	if reports[paneID] != "" {
		t.Fatalf("@tmux_web_agent = %q after an unknown state; want nothing written at all", reports[paneID])
	}
	if !strings.Contains(errb.String(), `unknown state "nonsense"`) {
		t.Errorf("stderr = %q, want it to name the state it did not recognise", errb.String())
	}
}

// A re-assertion's read, against a real server: the right pane, the right
// scope, and a real unset option.
//
// The stub tests decide WHETHER a read happens; only this one can decide what
// it reads, and the three things it has to get right are all invisible to a
// stub:
//
//   - `-t <pane>`. Dropped, the read lands on the CURRENT pane, which with no
//     client attached is the session's active one -- so the fixture reports on
//     the INACTIVE pane and leaves a disagreeing `working` standing on the
//     active one. A read that lost its target would see that, disagree, and
//     write.
//   - `-p`. Dropped, the read asks for a SESSION option and tmux answers
//     "invalid option" -- which is a disagreement, so it would write too.
//   - The unset option itself. tmux does not answer an unset user option with
//     an empty string; it exits 1 (measured on 3.7b, and the reason the error
//     path counts as a disagreement rather than as agreement with nothing).
//
// And the assertion is that the STORED VALUE DID NOT CHANGE, timestamp
// included. "Still idle" is not the claim -- a re-assertion that wrote
// 1;idle;<now> would satisfy that and would re-date the finish, which is the
// entire failure this task exists to prevent.
func TestAReassertionReadsThePaneItIsReportingOn(t *testing.T) {
	srv, reporting, other := twoAgentPanes(t)

	// The active pane carries a standing working, so a read that lost its
	// target or its scope finds a disagreement rather than nothing.
	srv.Run(t, "set", "-p", "-t", other, tmux.AgentOption, "1;working;1789075200000")

	idlePrompt := `{"hook_event_name":"Notification","notification_type":"idle_prompt"}`

	// (a) Nothing standing on the reporting pane: the option is unset, the read
	// fails, and the re-assertion writes. Without this half, a mutant that
	// treated every unreadable answer as agreement would look correct.
	var out, errb bytes.Buffer
	if code := runReport([]string{"--agent", "claude", "--event", "Notification"},
		strings.NewReader(idlePrompt), &out, &errb, paneEnv(srv, reporting), dialReal); code != 0 {
		t.Fatalf("report exited %d: %s", code, errb.String())
	}
	first := srv.Run(t, "show", "-p", "-t", reporting, "-v", tmux.AgentOption)
	if rep, ok := tmux.ParseReport(first, time.Now()); !ok || rep.State != tmux.StateIdle {
		t.Fatalf("@tmux_web_agent = %q -> %+v, %v; a re-assertion over an UNSET option must write", first, rep, ok)
	}

	// (b) The same event again, now that the pane agrees. Nothing may change.
	out.Reset()
	errb.Reset()
	if code := runReport([]string{"--agent", "claude", "--event", "Notification"},
		strings.NewReader(idlePrompt), &out, &errb, paneEnv(srv, reporting), dialReal); code != 0 {
		t.Fatalf("report exited %d: %s", code, errb.String())
	}
	if second := srv.Run(t, "show", "-p", "-t", reporting, "-v", tmux.AgentOption); second != first {
		t.Fatalf("@tmux_web_agent went from %q to %q. A re-assertion that agrees must write NOTHING: the "+
			"daemon derives finishedAt from the report's own timestamp, so a newer one is a new finish "+
			"and re-badges every device that had already seen this one", first, second)
	}
	// And the pane it must never have touched.
	if v := srv.Run(t, "show", "-p", "-t", other, "-v", tmux.AgentOption); v != "1;working;1789075200000" {
		t.Fatalf("the other pane's @tmux_web_agent = %q; the re-assertion wrote to the wrong pane", v)
	}
}

// -- fixture ----------------------------------------------------------------

// twoAgentPanes starts a throwaway server with two panes that the daemon will
// treat as agents, and returns the INACTIVE one first: that is the pane the
// tests report on, so that a write which forgot its target -- and therefore
// lands on the active pane -- is visible rather than silently correct.
func twoAgentPanes(t *testing.T) (srv *testutil.Server, reporting, other string) {
	t.Helper()
	agent := testutil.FakeAgent(t, "claude")
	srv = testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", agent)
	srv.Run(t, "split-window", "-t", "work", "-d", agent)

	var active, inactive []string
	for _, line := range strings.Split(srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_id} #{pane_active}"), "\n") {
		id, isActive, _ := strings.Cut(line, " ")
		if isActive == "1" {
			active = append(active, id)
		} else {
			inactive = append(inactive, id)
		}
	}
	if len(active) != 1 || len(inactive) != 1 {
		t.Fatalf("setup: %d active and %d inactive panes, want one of each", len(active), len(inactive))
	}
	waitForAgentPanes(t, srv)
	return srv, inactive[0], active[0]
}

// waitForAgentPanes blocks until tmux reports both panes as running the agent.
//
// Measured, and it is a real race rather than caution: for the first few
// milliseconds of a pane's life #{pane_current_command} reads "zsh" -- the
// process tmux forked has not reached its exec yet -- and it was seen as "tmux"
// too. The reports map does not care, since Task 3's read gives every pane a
// line whatever it is running, but the poller does: Reports.Observe drops a
// report unconditionally when KnownAgent(command) is empty, which is how it
// catches an agent that has exited. Without this wait the poller half of
// TestReportReachesTheSnapshot fails roughly one run in eight, and it fails by
// reporting no state at all -- which looks exactly like the precedence being
// broken.
func waitForAgentPanes(t *testing.T, srv *testutil.Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_current_command}")
		commands := strings.Split(out, "\n")
		ready := len(commands) == 2
		for _, cmd := range commands {
			if tmux.KnownAgent(cmd) == "" {
				ready = false
			}
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup: panes still running %q after 5s, want the fake agent in both", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// paneEnv is the environment an integration's hook would be handed inside that
// pane: $TMUX's first field is the socket, and $TMUX_PANE is the pane.
func paneEnv(srv *testutil.Server, paneID string) func(string) string {
	return mapEnv(map[string]string{
		"TMUX":      srv.SocketPath() + ",1,0",
		"TMUX_PANE": paneID,
	})
}

func rowFor(t *testing.T, rows []tmux.Row, paneID string) tmux.Row {
	t.Helper()
	for _, r := range rows {
		if r.PaneID == paneID {
			return r
		}
	}
	t.Fatalf("no row for pane %s in %+v", paneID, rows)
	return tmux.Row{}
}
