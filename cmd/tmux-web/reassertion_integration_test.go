package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

// The interleaving itself: three writes and a reader between them, against a
// real tmux server and the real daemon-side filter, with nothing stubbed.
//
// A unit test of the comparison proves the comparison. What broke here is a
// SEQUENCE -- the writer and the daemon holding two different answers to "what
// is standing" -- and the only thing that can show that is a run in which both
// of them look at the same option in turn.
//
// THE SEQUENCE, which is ordinary and needs no broken integration:
//
//  1. `Stop` writes 1;idle;T. The daemon accepts it, derives finishedAt = T,
//     and every enrolled device badges and stores T as `seen`.
//  2. Sixty seconds later `Notification(idle_prompt)` fires. Its process starts
//     -- which is where it stamps, never at write time -- and is descheduled
//     before it gets to tmux.
//  3. The user types. `UserPromptSubmit` is an EDGE: it reads nothing and
//     writes 1;working;T+, which lands first. The daemon accepts it; the pane
//     is working, and correctly so.
//  4. The re-assertion resumes and reads. The option says `working`, which
//     disagrees with the `idle` it would write, so the OLD writer wrote
//     1;idle;<its own older stamp>.
//  5. The daemon refuses that for being older -- and the pane is left holding
//     a resting report nobody will ever accept while this daemon lives. The
//     next FIRST SIGHT is not so lucky: a restart, a tmux-server generation
//     change, or a pane that left `keep` and came back reads the option cold,
//     accepts whatever it holds, and with no client connected derives
//     finishedAt from it immediately. finishedAt jumps to a value newer than
//     the T every device stored, so every device badges a finish -- on a pane
//     whose agent is in the middle of a turn.
//
// Step 3 is planted with `tmux set` rather than run through `report`, and that
// is the one thing here a test cannot get honestly: `report` stamps from
// time.Now() at process start, so two real processes racing inside one test
// always stamp in the order they run. A stamp AHEAD of the re-assertion's own
// is exactly what a deschedule produces, and planting it is how the deschedule
// is expressed. Everything else -- both other writes, the option, the parse,
// the ordering filter, the derivation -- is the real thing.
func TestALateReassertionLeavesNoStaleFinishOnAWorkingPane(t *testing.T) {
	srv, pane, _ := twoAgentPanes(t)
	c := tmux.NewClient(srv.Args())
	ctx := context.Background()

	// The live daemon's memory, carried across the whole sequence: this is the
	// filter that is supposed to absorb the reordering, and the point of the
	// test is that it absorbs it and the pane is still left wrong.
	live := tmux.NewReports()

	// (1) The turn end. A real edge write through the real subcommand.
	var out, errb bytes.Buffer
	if code := runReport([]string{"--agent", "claude", "--event", "Stop"},
		strings.NewReader(`{"hook_event_name":"Stop","background_tasks":[]}`),
		&out, &errb, paneEnv(srv, pane), dialReal); code != 0 {
		t.Fatalf("the Stop exited %d: %s", code, errb.String())
	}
	finish := observeReport(t, ctx, c, live, pane)
	if finish.State != tmux.StateIdle {
		t.Fatalf("after the Stop the daemon holds %+v, want an idle report", finish)
	}
	// What the browsers stored. finishedAt is derived from the report's own
	// timestamp, and each browser badges when finishedAt > seen.
	seen := finish.Timestamp

	// (3) The turn start that landed while the re-assertion was descheduled.
	// Two seconds ahead of the notification's stamp -- comfortably inside
	// ParseReport's five-second future skew, so the daemon reads it as an
	// ordinary report and not as a poisoned one.
	restart := strconv.FormatInt(time.Now().Add(2*time.Second).UnixMilli(), 10)
	srv.Run(t, "set", "-p", "-t", pane, tmux.AgentOption, "1;working;"+restart)
	if rep := observeReport(t, ctx, c, live, pane); rep.State != tmux.StateWorking {
		t.Fatalf("after the turn start the daemon holds %+v, want a working report", rep)
	}

	// (4) The re-assertion, resuming. Its own stamp is now, which is older than
	// the turn start it is about to look at.
	out.Reset()
	errb.Reset()
	if code := runReport([]string{"--agent", "claude", "--event", "Notification"},
		strings.NewReader(`{"hook_event_name":"Notification","notification_type":"idle_prompt"}`),
		&out, &errb, paneEnv(srv, pane), dialReal); code != 0 {
		t.Fatalf("the idle_prompt exited %d: %s", code, errb.String())
	}
	if stored := srv.Run(t, "show", "-p", "-t", pane, "-v", tmux.AgentOption); stored != "1;working;"+restart {
		t.Errorf("@tmux_web_agent = %q, want the turn start %q untouched: a re-assertion whose own "+
			"stamp is older than the standing report has nothing to say about this pane",
			stored, "1;working;"+restart)
	}

	// (5) The live daemon, which the ordering filter does protect.
	if rep := observeReport(t, ctx, c, live, pane); rep.State != tmux.StateWorking {
		t.Errorf("the live daemon holds %+v, want the working report still in force", rep)
	}

	// And the first sight, which it does not. A fresh memory is a restarted
	// daemon, a tmux-server generation change, or a pane that left `keep` and
	// came back; all three read the option cold and accept what it holds.
	//
	// Through the whole poller, because finishedAt is derived there and the
	// badge is the claim. Connected is false: nobody is watching, which is the
	// case the feature exists for and the case where the derivation is
	// immediate.
	p := tmux.NewPollerWith(tmux.Options{
		Interval:  time.Hour, // Start polls once synchronously
		Poll:      c.Poll,
		Capture:   c.Capture,
		Connected: func() bool { return false },
	})
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.Start(pctx)

	row := rowFor(t, p.Latest(), pane)
	if row.AgentState != tmux.StateWorking {
		t.Errorf("a restarted daemon reads this pane as %q, want working: the agent is in the "+
			"middle of a turn", row.AgentState)
	}
	if row.FinishedAt > seen {
		t.Errorf("finishedAt = %d, newer than the %d every device stored at the real finish. "+
			"That is a done badge on every enrolled device, once, for a turn that is still "+
			"running -- the badge storm the re-assertion kind was introduced to stop, arriving "+
			"through the writer instead of through the daemon", row.FinishedAt, seen)
	}
}

// observeReport runs one real poll and feeds this pane's raw option value
// through the daemon's own ordering filter, returning what is in force.
//
// The whole read path, not a shortcut: Poll's batched format read is what
// produces the raw value in production, and Observe is the filter under
// discussion. A test that parsed the option itself would be asserting against
// its own idea of the reader.
func observeReport(t *testing.T, ctx context.Context, c *tmux.Client, r *tmux.Reports, pane string) tmux.Report {
	t.Helper()
	polled, err := c.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	rep, ok := r.Observe(pane, polled.Reports[pane], rowFor(t, polled.Rows, pane).Command, time.Now())
	if !ok {
		t.Fatalf("no report in force for %s (option %q)", pane, polled.Reports[pane])
	}
	return rep
}
