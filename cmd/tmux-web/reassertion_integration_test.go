package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
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
	row := firstSight(t, ctx, c, pane)
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

// THE PURE JITTER RACE, end to end: the one ba544cd could not close, and the
// reason the repair is now dated from the standing report.
//
// It needs no broken integration and no unusual timing -- two fire-and-forget
// writes landing out of order is what the daemon's ordering filter exists for,
// and this is the case where absorbing it is not enough:
//
//  1. The turn runs. Its last working write is stamped T-d.
//  2. `Stop` at T writes idle;T. The daemon accepts it, derives finishedAt = T,
//     every enrolled device badges and stores T as `seen`. The user looks and
//     the badge clears.
//  3. THE WORKING WRITE FROM STEP 1 LANDS NOW. It was descheduled; it is
//     stamped T-d and the option becomes working;T-d. The daemon refuses it for
//     being older and goes on holding idle;T -- so nothing is wrong yet, and
//     nothing downstream can see that the option no longer says what the daemon
//     believes.
//  4. Sixty seconds later idle_prompt fires. It reads working;T-d: a state it
//     disagrees with, dated before its own stamp. Every clause of the old rule
//     says "repair this pane", and it is looking at bytes that are IDENTICAL to
//     a pane whose Stop was lost -- see the test below, where repairing is
//     right.
//
// Dated from NOW that repair writes idle;T+60, which is strictly newer than the
// idle;T the daemon holds, so the daemon takes it, finishedAt moves to T+60 and
// EVERY DEVICE THAT ALREADY SAW THIS FINISH BADGES AGAIN. Dated from the
// standing report it writes idle;(T-d)+1, which the daemon refuses for being
// older than what it holds -- and which a first sight reads as a finish OLDER
// than the one every device already stored, so nothing badges there either.
//
// Step 3 is planted with `tmux set` for the same reason step 3 of the test
// above is: `report` stamps from the clock at process start, so two real
// processes always stamp in the order they run, and a write that lands after a
// newer one is exactly what a deschedule produces. Everything else -- both real
// writes, the option, the parse, the ordering filter, the derivation -- is the
// real thing.
func TestALateWorkingWriteDoesNotLetTheNextRepairReBadgeAClearedFinish(t *testing.T) {
	srv, pane, _ := twoAgentPanes(t)
	c := tmux.NewClient(srv.Args())
	ctx := context.Background()
	live := tmux.NewReports()

	// (2) The turn end, through the real subcommand. Its stamp is T.
	runReportIn(t, srv, pane, []string{"--agent", "claude", "--event", "Stop"},
		`{"hook_event_name":"Stop","background_tasks":[]}`)
	finish := observeReport(t, ctx, c, live, pane)
	if finish.State != tmux.StateIdle {
		t.Fatalf("after the Stop the daemon holds %+v, want an idle report", finish)
	}
	// What every browser stored: finishedAt is derived from the report's own
	// timestamp, and a device badges when finishedAt > seen.
	seen := finish.Timestamp

	// (3) The turn's own last working write, descheduled past the Stop and
	// landing now. Two seconds BEFORE the Stop, which is what makes it a write
	// the daemon refuses rather than one it takes.
	stale := seen - 2000
	staleValue := "1;working;" + strconv.FormatInt(stale, 10)
	srv.Run(t, "set", "-p", "-t", pane, tmux.AgentOption, staleValue)
	if rep := observeReport(t, ctx, c, live, pane); rep.State != tmux.StateIdle || rep.Timestamp != seen {
		t.Fatalf("the daemon holds %+v after the late write, want the idle;%d it already accepted: "+
			"if the filter took this the test below is about a different pane", rep, seen)
	}

	// (4) The repair, resuming a minute later. It sees a working report it
	// disagrees with, older than its own stamp, and it repairs -- correctly, by
	// every clause it can evaluate.
	runReportIn(t, srv, pane, []string{"--agent", "claude", "--event", "Notification"},
		`{"hook_event_name":"Notification","notification_type":"idle_prompt"}`)
	stored := srv.Run(t, "show", "-p", "-t", pane, "-v", tmux.AgentOption)
	if want := "1;idle;" + strconv.FormatInt(stale+1, 10); stored != want {
		t.Errorf("@tmux_web_agent = %q, want %q: the repair is dated one millisecond after the "+
			"report it read, which is what makes it a compare-and-swap against the daemon's "+
			"strictly-newer filter", stored, want)
	}

	// (5) The live daemon refuses it, because it is holding news the option
	// lost. Nothing moves, and nothing was supposed to.
	if rep := observeReport(t, ctx, c, live, pane); rep.Timestamp != seen {
		t.Errorf("the live daemon holds %+v, want the idle;%d it accepted at the real turn end: "+
			"a repair dated from an option the daemon has already overruled must not become the "+
			"newest report on the pane", rep, seen)
	}

	// (6) And the first sight, which has no memory to protect it: a restarted
	// daemon, a tmux-server generation change, or a pane that left `keep` and
	// came back. It reads the option cold and derives finishedAt from whatever
	// it holds, with nobody connected -- the case this whole feature exists for.
	row := firstSight(t, ctx, c, pane)
	if row.AgentState != tmux.StateIdle {
		t.Errorf("a restarted daemon reads this pane as %q, want idle: the turn did end", row.AgentState)
	}
	if row.FinishedAt > seen {
		t.Errorf("finishedAt = %d, newer than the %d every device stored at the real finish. "+
			"That is a second done badge for one turn end, on every enrolled device -- the storm "+
			"the re-assertion kind exists to stop, arriving through a stale option the writer had "+
			"no way to tell from a lost Stop", row.FinishedAt, seen)
	}
}

// THE LOST STOP, end to end: the case the repair exists for, and the one whose
// bytes are indistinguishable from the race above.
//
// The turn ends and nothing writes it -- claude exited, the hook was killed,
// the wrapper never ran. The pane holds the turn's last working report and the
// daemon holds the same one; sixty seconds later the working expires and, with
// no client connected, the pane falls to a classifier with no screen to read.
// idle_prompt is what rescues it, and this is what must not regress: the repair
// has to LAND, and its finish has to badge.
//
// The finish it produces is dated at the turn's own last sign of life rather
// than sixty seconds after it, which is nearer the truth than dating from now
// ever was. What matters for the badge is only that it is newer than what the
// devices stored, and the last thing they stored was the PREVIOUS turn's
// finish, which is older than this turn's last working write by construction.
func TestARepairStillLandsWhenTheTurnEndWasLost(t *testing.T) {
	srv, pane, _ := twoAgentPanes(t)
	c := tmux.NewClient(srv.Args())
	ctx := context.Background()
	live := tmux.NewReports()

	// The turn start, through the real subcommand: a working edge, which is
	// what the pane is left holding when the turn end never arrives.
	runReportIn(t, srv, pane, []string{"--agent", "claude", "--event", "UserPromptSubmit"},
		`{"hook_event_name":"UserPromptSubmit"}`)
	work := observeReport(t, ctx, c, live, pane)
	if work.State != tmux.StateWorking {
		t.Fatalf("after the turn start the daemon holds %+v, want a working report", work)
	}

	// The turn ends. Nothing writes it. Sixty seconds later:
	runReportIn(t, srv, pane, []string{"--agent", "claude", "--event", "Notification"},
		`{"hook_event_name":"Notification","notification_type":"idle_prompt"}`)

	rep := observeReport(t, ctx, c, live, pane)
	if rep.State != tmux.StateIdle {
		t.Fatalf("the daemon holds %+v after the repair, want the idle it wrote: the repair is "+
			"the whole reason idle_prompt is mapped at all, and a compare-and-swap that cannot "+
			"land when the option IS what the daemon holds has closed the wrong case", rep)
	}
	if rep.Timestamp != work.Timestamp+1 {
		t.Errorf("the finish is dated %d, want %d -- one millisecond after the turn's last sign "+
			"of life, which is the earliest stamp the daemon's strictly-newer filter accepts over "+
			"it", rep.Timestamp, work.Timestamp+1)
	}

	// And the badge, through the whole poller: a device whose last stored value
	// is the PREVIOUS turn's finish sees a newer one and lights up.
	row := firstSight(t, ctx, c, pane)
	if row.AgentState != tmux.StateIdle {
		t.Errorf("the pane reads as %q, want idle", row.AgentState)
	}
	if row.FinishedAt != work.Timestamp+1 {
		t.Errorf("finishedAt = %d, want %d: this turn's badge is the whole point of the repair",
			row.FinishedAt, work.Timestamp+1)
	}
}

// runReportIn runs one real report process against this pane, through the real
// subcommand and the real tmux client.
func runReportIn(t *testing.T, srv *testutil.Server, pane string, args []string, stdin string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := runReport(args, strings.NewReader(stdin), &out, &errb, paneEnv(srv, pane), dialReal); code != 0 {
		t.Fatalf("report%v exited %d: %s", args, code, errb.String())
	}
}

// firstSight is one poll by a daemon with no memory of this pane and no client
// connected: a restart, a tmux-server generation change, or a pane that left
// `keep` and came back.
//
// Through the whole poller rather than through Reports alone, because
// finishedAt is derived there and the badge is the claim. Connected is false
// because that is the case the feature exists for, and the case where the
// derivation is immediate.
func firstSight(t *testing.T, ctx context.Context, c *tmux.Client, pane string) tmux.Row {
	t.Helper()
	p := tmux.NewPollerWith(tmux.Options{
		Interval:  time.Hour, // Start polls once synchronously
		Poll:      c.Poll,
		Capture:   c.Capture,
		Connected: func() bool { return false },
	})
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.Start(pctx)
	return rowFor(t, p.Latest(), pane)
}
