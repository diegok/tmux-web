package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The tmux server's generation rides the batched read.
//
// The design's rule is one fork, N format strings, and the generation was the
// one thing breaking it: display-message ran beside the batch on every poll,
// unconditionally, for a value tmux is perfectly willing to print inside the
// invocation the poller already makes. Measured on tmux 3.7b on this machine,
// over three runs of 100 polls: 4.9-5.2ms per poll as two invocations against
// 2.5-2.8ms as one -- the fork is the whole cost, so removing one halves it.

// --- layer 1, as arguments ---------------------------------------------------

func TestBatchArgsReadsTheGenerationInTheSameFork(t *testing.T) {
	args := batchArgs()
	if !slices.Contains(args, StartFormat) {
		t.Fatalf("batchArgs = %q does not read the generation", args)
	}
	var invocations int
	for _, a := range args {
		if a == "display-message" {
			invocations++
		}
	}
	if invocations != 1 {
		t.Errorf("batchArgs runs display-message %d times, want exactly 1: %q", invocations, args)
	}
	// Four commands, so three separators -- and still one argv, which is the
	// point. A second c.Run for the generation would satisfy every other
	// assertion here and cost a fork per poll forever.
	if got := strings.Count(strings.Join(args, " "), " ; "); got != 3 {
		t.Errorf("batchArgs has %d command separators, want 3 for four commands in one invocation: %q", got, args)
	}
	// A tag every other block already uses would make the generation line
	// parse as one of theirs, and one of theirs parse as the generation.
	if startTag == snapshotTag || startTag == reportTag || startTag == pathTag {
		t.Errorf("startTag %q collides with one of %q, %q, %q", startTag, snapshotTag, reportTag, pathTag)
	}
}

// Last, behind everything that must not be lost.
//
// tmux runs a command list in order and stops at the first failure --
// TestTheSnapshotBlockRunsFirst measures that against a real server -- so
// anything ahead of the snapshot block can blank the sidebar. Measured on 3.7b,
// display-message is hard to make fail at all (`-p -t nosuch` prints the
// current session's answer and exits 0), which is a reason not to worry about
// this block rather than a reason to put it anywhere: the rule is that the
// snapshot never stands behind a command that could fail, and a block whose
// failure modes are unknown is exactly the kind that belongs at the end.
func TestTheGenerationBlockRunsLast(t *testing.T) {
	args := batchArgs()
	if !slices.Contains(args, ";") {
		t.Fatalf("batchArgs = %q is not a command list", args)
	}
	last := args[len(args)-3:]
	if last[0] != "display-message" || last[2] != StartFormat {
		t.Errorf("the last command of batchArgs is %q, want the generation block: nothing that reads "+
			"the panes may stand behind a block whose failure would abort the list", last)
	}
}

// --- layer 2, against a real tmux --------------------------------------------

func TestTheBatchCarriesTheServerGeneration(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	c := NewClient(srv.Args())

	got, err := c.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("Poll returned %d rows, want the one pane of the fixture", len(got.Rows))
	}
	if !got.HaveServerStart {
		t.Fatal("the batch reported no generation against a running server")
	}
	// The same answer tmux gives when asked on its own, which is the whole
	// claim: the second fork bought nothing.
	if want := srv.Run(t, "display-message", "-p", "#{start_time}"); got.ServerStart != want {
		t.Errorf("the batched generation is %q, tmux says %q", got.ServerStart, want)
	}
	alone, err := c.ServerStart(context.Background())
	if err != nil || alone != got.ServerStart {
		t.Errorf("ServerStart() = %q, %v; the batch read %q -- the two readers must not disagree",
			alone, err, got.ServerStart)
	}
}

// No server is an ANSWER about the generation, not a missing one. "" is what
// this daemon reports for a machine with no tmux running, and the poller has to
// see it arrive: the reset of everything keyed on a pane id hangs off the
// generation changing, and a server that went away has taken its pane ids with
// it.
func TestTheBatchOnNoServerReportsAnEmptyGenerationItKnows(t *testing.T) {
	srv := testutil.NewServer(t) // never started
	got, err := NewClient(srv.Args()).Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll with no server = %v, want no error", err)
	}
	if len(got.Rows) != 0 {
		t.Fatalf("Poll with no server returned %d rows", len(got.Rows))
	}
	if got.ServerStart != "" || !got.HaveServerStart {
		t.Errorf("Poll with no server = {%q, have:%v}, want a KNOWN empty generation",
			got.ServerStart, got.HaveServerStart)
	}
}

// A generation block that failed costs the generation and nothing else. The
// snapshot is what the sidebar is, and it is already complete on stdout by the
// time a later block fails -- the rule runKeepingOutput exists for.
//
// The argv is swapped rather than the server broken, for the reason
// report_internal_test.go gives: nothing else can make the LAST block fail
// while the first succeeds against a real server.
func TestABrokenGenerationReadStillYieldsTheSnapshot(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	orig := batchArgs
	t.Cleanup(func() { batchArgs = orig })
	batchArgs = func() []string {
		return []string{
			"list-panes", "-a", "-F", Format,
			// list-panes rather than display-message: it is the one that
			// really fails on a target it cannot find, which is what a broken
			// last block has to do here.
			";", "list-panes", "-t", "nosuch", "-F", StartFormat,
		}
	}

	got, err := NewClient(srv.Args()).Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll = %v; a failed generation block must not fail the poll", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("Poll returned %d rows, want the pane the first block listed", len(got.Rows))
	}
	if got.HaveServerStart {
		t.Errorf("Poll reported generation %q from a read that carried none; the poller would take it "+
			"for a restart and drop every pane's memory", got.ServerStart)
	}
}

// --- layer 3, as parser ------------------------------------------------------

func TestParseServerStart(t *testing.T) {
	line := func(tag string, fields ...string) string {
		return strings.Join(append([]string{tag}, fields...), Sep)
	}
	snapshot := line(snapshotTag, "work", "$0", "work", "%0", "0", "", "@0", "0", "win", "1", "zsh", "title", "label")

	t.Run("the generation among the other blocks", func(t *testing.T) {
		out := strings.Join([]string{
			snapshot,
			line(reportTag, "%0", "1;working;123"),
			line(pathTag, "%0", "/home/x"),
			line(startTag, "1789038099"),
		}, "\n")
		got, ok := ParseServerStart(out)
		if !ok || got != "1789038099" {
			t.Errorf("ParseServerStart = %q, %v; want the generation the last block printed", got, ok)
		}
	})

	t.Run("no generation line at all", func(t *testing.T) {
		got, ok := ParseServerStart(strings.Join([]string{snapshot, line(pathTag, "%0", "/home/x")}, "\n"))
		if ok || got != "" {
			t.Errorf("ParseServerStart = %q, %v on a read with no generation block; want \"\", false -- "+
				"which is what keeps the last known generation in place", got, ok)
		}
	})

	t.Run("nothing else is mistaken for it", func(t *testing.T) {
		// A path or a label ending up read as a generation would be a restart
		// on every poll, and a restart drops every pane's memory.
		if got, ok := ParseServerStart(line(pathTag, "%0", "/home/G")); ok {
			t.Errorf("ParseServerStart read %q out of a path line", got)
		}
	})

	t.Run("empty output", func(t *testing.T) {
		if got, ok := ParseServerStart(""); ok || got != "" {
			t.Errorf("ParseServerStart(\"\") = %q, %v", got, ok)
		}
	})
}

// The generation line is not a pane and must not be counted as a lost one.
// ParseRows logs "skipped malformed rows" per poll on a dropped count, and a
// warning that fires 40 times a minute forever is a warning nobody reads on the
// day a pane really does go missing.
func TestParseRowsSkipsTheGenerationLine(t *testing.T) {
	fields := []string{
		snapshotTag, "work", "$0", "work", "%0", "0", "", "@0", "0", "win", "1", "zsh", "title", "label",
	}
	out := strings.Join([]string{
		strings.Join(fields, Sep),
		strings.Join([]string{startTag, "1789038099"}, Sep),
	}, "\n")

	rows, dropped, err := ParseRows(out)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ParseRows returned %d rows, want the one pane", len(rows))
	}
	if dropped != 0 {
		t.Errorf("ParseRows counted the generation line as %d malformed rows", dropped)
	}
}

// --- layer 4, as a fork count ------------------------------------------------

// The claim this whole block exists for, measured rather than argued: one poll
// against a real tmux server forks tmux ONCE.
//
// Everything above pins the arguments and the parsing, and all of it stays
// green if somebody reads the generation with a second c.Run beside a batch
// that also carries it -- the two answers would agree, so nothing would look
// wrong. Counting invocations is the only assertion that can see the
// difference, and the fork is the cost: measured on this machine, a poll costs
// 4.9-5.2ms as two invocations against 2.5-2.8ms as one.
//
// The poller is wired as the daemon wires it, with no browser connected, which
// is what "the unconditional cost of a poll" means: the captures are the part
// that is gated on somebody watching.
func TestOnePollForksTmuxOnce(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux not found: %v", err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations")
	shim := "#!/bin/sh\necho x >> " + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	// After the fixture is seeded, so only the poll is counted.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := NewClient(srv.Args())
	p := NewPollerWith(Options{
		Interval:  time.Hour,
		Poll:      c.Poll,
		Capture:   c.Capture,
		Connected: func() bool { return false },
	})
	p.refresh(context.Background())

	if err := p.Err(); err != nil {
		t.Fatalf("the poll failed, so this counted nothing: %v", err)
	}
	if len(p.Latest()) != 1 || p.ServerStart() == "" {
		t.Fatalf("the poll did not land: rows %+v, generation %q", p.Latest(), p.ServerStart())
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the shim was never run, so PATH did not reach the client: %v", err)
	}
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Errorf("one poll forked tmux %d times, want 1: the rows, the reports, the paths and the "+
			"generation are one invocation, and a second one costs a fork on every poll forever", got)
	}
}
