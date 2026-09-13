package tmux

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Client runs tmux commands against one server.
type Client struct {
	// base is prepended to every invocation, e.g. []string{"-L", "sockname"}.
	// Empty means the user's default server.
	base []string
	// paths answers a pane's working directory from a snapshot somebody else
	// already took; see UsePathCache. nil means every caller asks tmux.
	paths func(paneID string) (string, bool)
}

func NewClient(base []string) *Client { return &Client{base: base} }

// UsePathCache points this client at an already-polled answer to "where is this
// pane", so that a split does not fork tmux to re-read a value the daemon is
// holding. Poller.PathFor is what the daemon passes.
//
// Not a constructor argument because the poller is built FROM this client: the
// daemon wires the two together once, at startup, before either is serving
// anything. It is not safe to call again afterwards, and there is no reason to.
//
// A cache is a hint and never an authority. What it answers is up to a poll
// interval old and may name a directory that has since been removed, so
// panePath stats it and falls back to a live read; see there.
func (c *Client) UsePathCache(f func(paneID string) (string, bool)) { c.paths = f }

func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", append(append([]string{}, c.base...), args...)...)
}

// runKeepingOutput is Run for a command whose stdout is worth having even when
// it fails.
//
// It exists for exactly one caller. SnapshotAndReports runs two commands in one
// invocation, and a failure in the second leaves the first's output complete on
// stdout -- so a nonzero exit there is a missing REPORT, not a missing
// snapshot, and discarding stdout would trade a degraded feature for a blank
// sidebar.
//
// stdout and stderr are captured separately, exactly as in the test harness and
// for the same reason: the callers split this return value on 0x1f and index
// fields positionally, so a single diagnostic line merged into it would produce
// a malformed row and fail far from its cause. stderr goes into the error,
// where it is useful, rather than into data that gets parsed.
func (c *Client) runKeepingOutput(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := c.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimRight(stdout.String(), "\n")
	if err != nil {
		if msg := strings.TrimRight(stderr.String(), "\n"); msg != "" {
			return out, fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Run executes a tmux command and returns trimmed stdout.
//
// The contract is unchanged from before runKeepingOutput existed and every
// caller here depends on it: on failure it returns "" and an error carrying
// stderr, which is what noServer() matches on.
func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	out, err := c.runKeepingOutput(ctx, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// batchArgs is the one tmux invocation the poller makes per refresh: the
// snapshot, then the reports, then the working directories, then the server's
// generation, in command order.
//
// A lone ";" argv element is tmux's own command separator -- the same shape
// AttachArgs already uses. There is no shell here, so it needs no escaping.
//
// It is a var rather than a func for exactly one reason: SnapshotAndReports
// takes no arguments and calls this itself, so this is the only seam through
// which a test can make the SECOND command fail while the first succeeds. That
// is what TestABrokenReportReadStillYieldsTheSnapshot swaps, and without the
// seam the rule "parse stdout on its own terms, do not gate on the exit status"
// has no test that can see it. Nothing in production reassigns it.
var batchArgs = func() []string {
	return []string{
		"list-panes", "-a", "-F", Format,
		";", "list-panes", "-a", "-F", ReportFormat,
		";", "list-panes", "-a", "-F", PathFormat,
		";", "display-message", "-p", StartFormat,
	}
}

// Poll is everything one refresh needs from tmux, read in one fork.
//
// A struct rather than a widening list of return values, and it exists for the
// last field: the generation is "" both when the server is gone and when the
// block that reads it failed, and those are opposite instructions to the
// poller.
type Poll struct {
	// Rows is one row per pane, deduplicated across session groups, each
	// carrying the pane's working directory.
	Rows []Row
	// Reports is every pane's raw @tmux_web_agent value, keyed by pane id.
	Reports map[string]string
	// ServerStart is the tmux server's generation; see Client.ServerStart for
	// what anything keyed on a pane id needs it for.
	ServerStart string
	// HaveServerStart is whether this read carried a generation at all. False
	// means the block did not arrive, and the caller must keep whatever it
	// already knew rather than reading ServerStart's "" as a restart.
	HaveServerStart bool
}

// Poll returns one row per pane -- each carrying the pane's working directory
// -- every pane's raw @tmux_web_agent value, and the tmux server's generation,
// from a single tmux invocation.
//
// The marginal cost of each block after the first is zero forks: they are more
// commands inside a fork the poller already makes unconditionally. Against
// them, the reports remove one capture-pane fork per reporting agent pane per
// poll, the paths remove the list-panes fork panePath used to make on every
// split and every new window -- a keystroke-initiated action, where the
// latency is the one the owner can feel -- and the generation removes the one
// unconditional second fork the poll still had. There is no poll at which any
// of them costs a fork it did not save.
//
// Measured on tmux 3.7b on the development machine, over three runs of 100
// polls against a two-pane server: 2.5-2.8ms per poll for all four blocks in
// one invocation, against 4.9-5.2ms for the same three blocks plus a separate
// display-message. The fork is the cost, so removing one halves it.
func (c *Client) Poll(ctx context.Context) (Poll, error) {
	out, err := c.runKeepingOutput(ctx, batchArgs()...)
	if err != nil && noServer(err.Error()) {
		// No panes, and a generation that is KNOWN to be empty -- not a
		// generation that failed to arrive. The distinction is the poller's
		// per-pane memory: a server that has gone away has taken its pane ids
		// with it, and the change from a real generation to "" is what drops
		// everything keyed on them.
		return Poll{HaveServerStart: true}, nil
	}
	rows, dropped, perr := ParseRows(out)
	if perr != nil {
		return Poll{}, perr
	}
	// The exit status is NOT the gate. A nonzero exit with a complete snapshot
	// block is a missing report or a missing set of paths; only a nonzero exit
	// with nothing usable on stdout is a failed poll. With three blocks that
	// rule matters more, not less: it is one more way for a whole sidebar to go
	// blank over a feature that degrades perfectly well on its own.
	if err != nil && len(rows) == 0 {
		return Poll{}, err
	}
	if err != nil {
		slog.Warn("tmux batch: a later block failed; the snapshot is intact", "error", err)
	}
	// Attached before Dedupe so that every copy of a pane carries it, whichever
	// of them Dedupe keeps. A pane with no line in the path block gets "",
	// which is what panePath treats as "ask tmux".
	paths := ParsePaths(out)
	for i := range rows {
		rows[i].Path = paths[rows[i].PaneID]
	}
	if dropped > 0 {
		slog.Warn("tmux snapshot: skipped malformed rows", "dropped", dropped)
	}
	start, haveStart := ParseServerStart(out)
	return Poll{
		Rows:            Dedupe(rows),
		Reports:         ParseReports(out),
		ServerStart:     start,
		HaveServerStart: haveStart,
	}, nil
}

// Snapshot returns one row per pane, deduplicated across session groups.
//
// Row.Path is "" on every row: this is the single-command read, and the path
// rides a block of its own inside the batched one. Nothing is broken by that --
// panePath falls back to asking tmux when it has no cached answer -- but a
// poller built on this function saves no fork.
func (c *Client) Snapshot(ctx context.Context) ([]Row, error) {
	out, err := c.Run(ctx, "list-panes", "-a", "-F", Format)
	if err != nil {
		// No server running is not an error condition for the UI. tmux reports
		// this on stderr, which Run folds into the error message.
		if noServer(err.Error()) {
			return nil, nil
		}
		return nil, err
	}
	rows, dropped, err := ParseRows(out)
	if err != nil {
		return nil, err
	}
	if dropped > 0 {
		// A pane silently missing from the sidebar is the worst failure this
		// project has, so an unparseable row is logged rather than swallowed.
		// It is not promoted to an error: the panes that did parse are still
		// worth showing.
		slog.Warn("tmux snapshot: skipped malformed rows", "dropped", dropped)
	}
	return Dedupe(rows), nil
}

// startFormatFields is the fourth and last block of the batched read.
//
// display-message rather than a fourth list-panes, because the generation
// belongs to the server and not to a pane: asked through list-panes it would
// arrive once per pane, and the parser would have to decide which copy to
// believe. It is also the only block whose format carries no field anything
// else can write -- #{start_time} is tmux's own clock, in digits -- so it needs
// none of the defences the label, the report and the path get.
//
// The format is the argument to -p, not to -F, which is display-message's own
// spelling of the same thing.
var startFormatFields = []string{startTag, "#{start_time}"}

// StartFormat is the -p argument for the generation block.
var StartFormat = strings.Join(startFormatFields, Sep)

// ParseServerStart pulls the tmux server's generation out of the batched read,
// and reports whether the read carried one at all.
//
// The bool is the whole point of the signature: "" is a legitimate generation
// -- it is what no server at all reports -- so a caller cannot tell a read that
// lost its last block from a machine with no tmux on it by looking at the
// value. They mean opposite things to the poller: one keeps the last known
// generation, the other is a change that drops every pane's remembered state.
func ParseServerStart(out string) (string, bool) {
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		tag, value, ok := strings.Cut(line, Sep)
		if !ok || tag != startTag {
			continue
		}
		return strings.TrimSpace(value), true
	}
	return "", false
}

// ServerStart identifies the generation of the running tmux server, or "" if
// there is none.
//
// Pane ids are only unique within one server's life: they restart at %0 when the
// server does. Anything remembering something per pane across polls -- the
// browser's "this finished and I have not looked yet" memory -- has to key on
// this too, or a remembered %3 is silently applied to an unrelated new pane.
//
// The value is tmux's own #{start_time}, in whole seconds, and it is treated as
// an opaque token rather than a time. Two servers started within the same second
// therefore share a generation; that is a restart so fast it cannot be produced
// by hand, and the consequence is one stale badge, so it is not worth a second
// discriminator that tmux does not offer.
//
// No server is not an error, for the same reason as in Snapshot: a machine where
// tmux has never started is an ordinary cold start, not a fault.
//
// This is the fork Poll removes, and it survives for the pollers that have no
// batch to carry the value -- NewPoller's, over the single-command Snapshot.
// Wiring it beside Poll is refused; see NewPollerWith.
func (c *Client) ServerStart(ctx context.Context) (string, error) {
	out, err := c.Run(ctx, "display-message", "-p", "#{start_time}")
	if err != nil {
		if noServer(err.Error()) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// noServer reports whether a tmux failure means "there is no server on this
// socket" rather than a real fault.
//
// tmux 3.7b says this two different ways, depending on what is left of the
// socket. Once the server has exited but its socket file survives, connect
// fails with ECONNREFUSED and tmux prints "no server running on <path>". When
// the file was never created -- a machine where tmux has not been started since
// boot, and the state of every fresh test server -- connect fails with ENOENT
// and tmux prints "error connecting to <path> (No such file or directory)".
// Both mean the same thing to the sidebar: no panes.
//
// The ENOENT text is matched alongside the prefix rather than the prefix alone,
// because "error connecting to" also covers failures the operator must see --
// a permission error on a socket owned by another user, say, which as an empty
// sidebar would be indistinguishable from a machine with no tmux running.
//
// Matching on message text is fragile: tmux exits 1 for this and for genuine
// errors alike, so there is nothing else to match on. TestSnapshotWithNoServer*
// pin both strings against a real tmux.
func noServer(msg string) bool {
	return strings.Contains(msg, "no server running") ||
		(strings.Contains(msg, "error connecting to") &&
			strings.Contains(msg, "No such file or directory"))
}

// SelectPane points one session at a pane: the browser tab's own grouped
// session, so clicking a pane in one tab moves neither the other tabs nor the
// terminal the user is sitting in front of.
//
// The plan specified `select-window -t <session>:<paneID>`, and tmux 3.7b
// rejects it -- "can't find window: %3". A window target parses <session>:<win>
// and looks <win> up as a name, index or @id; a pane id is not one of those.
// The bare form `select-window -t %3` does work, but it picks the session
// itself, and with grouped sessions that choice is arbitrary: with two tabs
// open on one base, it moved the wrong one. So the pane's window id has to be
// in hand, to qualify with the session that must move. "=" pins the session
// name to an exact match, as in Sweep.
//
// windowID is where that id comes from, and it is a HINT: the caller's most
// recent snapshot already carries one per row (Row.WindowID, via
// Poller.WindowFor), and the row it carries is the row the user clicked. With
// it, the whole click is ONE invocation -- the read, the select-window and the
// select-pane chained with ";" -- against the three it used to cost. Measured
// on this machine: 10.1-11.3ms per click as three invocations, 5.9-6.5ms as
// two, 3.3-4.2ms as one.
//
// THE HINT SAVES THE FORKS, NOT THE READ. It is a poll interval old, and a pane
// moved between windows since -- break-pane, join-pane -- would send the tab to
// a window that no longer holds what it clicked, with no error to notice. So
// the read stays in the chain and costs nothing extra, its answer is compared
// against the hint, and a hint that does not match is corrected by a second
// invocation rather than believed. A hint that is not a window id at all is
// dropped before tmux ever sees it, for the reason every id here is validated:
// tmux resolves a great many strings to "whatever is current" and exits 0.
//
// Reading FIRST is what keeps the failure behaviour. tmux abandons the rest of
// a command list once one of its commands fails, so a pane that died between
// the poll and the click fails the read and the two selects never run -- the
// same "error, and nothing moved" the un-chained version gave, which
// TerminalHandler's "where" reply depends on.
//
// list-panes does the reading rather than display-message, which is the obvious
// command and is unusable here: given a target it cannot find, it prints an
// empty expansion and exits 0, and `display-message -t work:9` happily answers
// about a different window. list-panes errors on all of those. It reports the
// window id once per pane in the window; the lines are identical and the first
// is taken.
//
// Only the current window is per-session. The active pane belongs to the
// window, which grouped sessions share, so that half is visible to every member
// of the group. tmux offers no way to scope it, and it matches what the user
// sees when they select a pane in one of two attached clients.
func (c *Client) SelectPane(ctx context.Context, session, paneID, windowID string) error {
	// tmux resolves an empty target to "whatever is current" and exits 0, so an
	// unset pane id would quietly navigate the tab somewhere arbitrary instead
	// of failing. The ids come from the frontend, where "no selection yet" is
	// one bug away from being the empty string.
	if err := ValidatePaneID(paneID); err != nil {
		return fmt.Errorf("select pane: %w", err)
	}
	if session == "" {
		return fmt.Errorf("select pane %s: no session given", paneID)
	}

	read := []string{"list-panes", "-t", paneID, "-F", "#{window_id}"}
	if ValidateWindowID(windowID) == nil {
		out, err := c.Run(ctx, append(read, append([]string{";"}, selectWindowPaneArgs(session, windowID, paneID)...)...)...)
		if err != nil {
			return err
		}
		actual, _, _ := strings.Cut(out, "\n")
		if actual == windowID {
			return nil
		}
		// The hint was stale. The selects above landed the tab on a real window
		// of its own session, so the correction below is a move, not a repair of
		// something broken -- and it is the read's answer, not another guess.
		windowID = actual
	} else {
		out, err := c.Run(ctx, read...)
		if err != nil {
			return err
		}
		windowID, _, _ = strings.Cut(out, "\n")
	}
	_, err := c.Run(ctx, selectWindowPaneArgs(session, windowID, paneID)...)
	return err
}

// selectWindowPaneArgs is the two-command tail of a select: point the session at
// the window, then the window at the pane. One invocation, because tmux runs a
// ";"-separated command list in the client it already started -- the second
// command is free, and it used to be a fork.
func selectWindowPaneArgs(session, windowID, paneID string) []string {
	return []string{
		"select-window", "-t", "=" + session + ":" + windowID,
		";", "select-pane", "-t", paneID,
	}
}

// CurrentPane answers "which pane is this session looking at": the active pane
// of the session's own current window.
//
// It is the read half of SelectPane and exists because a browser tab cannot
// work this out for itself. The current window belongs to the *session*, which
// is the whole reason each tab gets a throwaway session grouped onto the user's
// real one -- so `#{window_active}` in a snapshot row answers for whichever
// session that row was listed under, and the snapshot the browser gets is
// deduplicated to one row per pane with the user's own session preferred. Read
// from there, "the active pane of the active window" is the pane the *local
// terminal* is sitting on, which is exactly the pane the browser tab is not.
// Measured on 3.7b: with the base session on window 0 and its grouped member on
// window 2, `list-panes -a` reports window_active=1 on window 0 for the base and
// on window 2 for the member, for the same shared windows.
//
// list-panes rather than display-message, for the reason SelectPane gives at
// length and re-measured here: `display-message -p -t '=nosuch:' '#{pane_id}'`
// prints an empty line and exits 0, while list-panes exits 1 with "can't find
// session". This runs against a session that may not have been created yet --
// see ptybridge.Session.CurrentPane -- so an error is the answer that has to be
// distinguishable from a pane id, not one that has to be guessed at from an
// empty string.
//
// The "-f" filter keeps the active pane. Without it this returns whichever pane
// tmux lists first, which is the same answer for an unsplit window and a
// different one the moment the user splits -- a bug that would look like it
// works. The result is validated for the same reason every id here is: a
// format that stopped expanding would otherwise hand "" to the browser as a
// pane it is looking at.
func (c *Client) CurrentPane(ctx context.Context, session string) (string, error) {
	if session == "" {
		// tmux resolves an empty target to "whatever is current" and exits 0,
		// so an unset session would answer about an arbitrary one.
		return "", fmt.Errorf("current pane: no session given")
	}
	out, err := c.Run(ctx, "list-panes", "-t", "="+session+":", "-F", "#{pane_id}", "-f", "#{pane_active}")
	if err != nil {
		return "", err
	}
	// One line per active pane, and a window has exactly one. Cut rather than
	// trust: a window that somehow reported two would otherwise return both
	// joined by a newline, which no validator would accept and no caller wants.
	pane, _, _ := strings.Cut(out, "\n")
	if err := ValidatePaneID(pane); err != nil {
		return "", fmt.Errorf("current pane of %s: %w", session, err)
	}
	return pane, nil
}

// IsMissingSession reports whether err is tmux saying the session does not
// exist, as opposed to any other failure.
//
// Matched on message text for the same reason as noServer: tmux exits 1 for
// every failure alike, so the text is the only discriminator there is. It is
// exported because ptybridge waits out exactly this error while a freshly
// spawned attach creates its session, and must not wait out any other.
func IsMissingSession(err error) bool {
	return err != nil && strings.Contains(err.Error(), "can't find session")
}

// KillSession removes a throwaway session explicitly. destroy-unattached is the
// crash net; this is the normal teardown path.
//
// "=" pins the target to an exact match, for the reason spelled out in Sweep:
// tmux falls back to prefix matching without reporting the ambiguity, so
// `kill-session -t _web-` would kill an unrelated _web-abcd and exit 0. Unlike
// Sweep, this takes a name from its caller rather than from tmux, so the guard
// is reachable and tested.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.Run(ctx, "kill-session", "-t", "="+name)
	return err
}

// Capture returns a pane's visible screen.
//
// -p writes it to stdout instead of a buffer, and -J rejoins a line the pane
// wrapped so that a question broken across two rows reads as one.
//
// Deliberately NOT -S -8. A negative -S counts back from the top of the visible
// screen into scrollback, so on a 6-row pane `-S -8` returns 14 lines: the
// screen plus 8 lines of history, which is exactly where a just-answered
// approval box lives. Reporting one of those as a live question is the false
// positive that would train the owner to ignore the badge. The visible screen
// is what is being asked about, and any slicing happens in Go.
//
// The whole screen is returned rather than a tail: the hash of it is what tells
// working from idle, and a redraw at the top of the screen is work too.
func (c *Client) Capture(ctx context.Context, paneID string) (string, error) {
	// tmux resolves an empty target to "whatever is current" and exits 0, so an
	// unvalidated id would silently classify some other pane -- and the state
	// would look plausible while belonging to the wrong row.
	if err := ValidatePaneID(paneID); err != nil {
		return "", fmt.Errorf("capture pane: %w", err)
	}
	return c.Run(ctx, "capture-pane", "-p", "-J", "-t", paneID)
}

// MaxCaptureBytes bounds one scrollback capture.
//
// The cap is on BYTES and not on lines, because a line is not a unit of size: a
// 200-column pane's line is worth twice an 80-column pane's, and a full-width
// 5000-line history at 200 columns is about 1 MB. Measured on a 200x50 pane
// with a 5000-line history of ~88-character lines: the visible screen is 3904
// bytes and 2.6 ms, `-S -2000` is 167 904 bytes and 5.1-5.7 ms, and the whole
// history is 327 227 bytes and 7.6-8.0 ms. Depth is not what costs; the fork is.
const MaxCaptureBytes = 256 << 10

// maxCaptureLines is the deepest scrollback a caller may ask for.
const maxCaptureLines = 5000

// CaptureRange returns a pane's scrollback plus its visible screen, bounded.
//
// Separate from Capture, which the classifier owns and which deliberately has
// no -S: a negative start line reaches into scrollback where a just-answered
// approval box lives, and that is a false `blocked`. That reasoning is about
// the classifier and does not transfer to a panel, so the panel gets its own
// call rather than widening the classifier's.
//
// Truncation is from the TOP, on a rune boundary. The newest lines are the ones
// the panel was opened for, and cutting the tail would throw away the answer to
// keep the question. truncated says whether that happened, so the panel can say
// so rather than showing a capture that silently begins mid-sentence.
func (c *Client) CaptureRange(ctx context.Context, paneID string, lines int) (text string, truncated bool, err error) {
	// As in Capture: tmux resolves an empty target to "whatever is current"
	// and exits 0, so an unvalidated id shows the browser some other pane's
	// scrollback under this pane's name.
	if err := ValidatePaneID(paneID); err != nil {
		return "", false, fmt.Errorf("capture range: %w", err)
	}
	out, err := c.Run(ctx, captureRangeArgs(paneID, lines)...)
	if err != nil {
		return "", false, err
	}
	text, truncated = capCapture(out)
	return text, truncated, nil
}

// capCapture applies MaxCaptureBytes to one capture and reports whether it bit.
//
// Split out from CaptureRange because the boundary is the only part of this
// that can be tested without a tmux server, and an off-by-one here is a panel
// that claims a complete capture was truncated -- or, the other way, one that
// silently drops a byte and says nothing.
func capCapture(s string) (text string, truncated bool) {
	if len(s) <= MaxCaptureBytes {
		return s, false
	}
	return truncateHeadAtRuneBoundary(s, MaxCaptureBytes), true
}

// captureRangeArgs builds the command line, clamped.
//
// The clamp is here as well as in the handler, and the two are about different
// things: the handler's is about rejecting a bad request, this one is about the
// method being safe to call from anywhere. Its own test reads these args
// because the output cannot show them -- tmux clamps a start line to the
// history it has, so an over-deep -S returns exactly what a correct one does.
//
// -S -<N> is N lines of scrollback PLUS the visible screen. It must stay
// negative: a positive start line counts from the top of the history instead of
// back from the screen, and tmux reports no error for it.
//
// -J is kept from Capture: it rejoins a line the pane wrapped, so a URL split
// across rows copies as one string.
//
// -N is NOT a numeric start line, which is the reading its name invites; it
// means "preserve trailing spaces". Measured on 3.7b, and contrary to the
// design's note, it is a NO-OP next to -J -- which preserves them already:
// `-p -J -N` and `-p -J` came back byte-identical (286 bytes) on a 40-column
// fixture, while the same capture without -J went from 284 bytes to 452 padded.
// So the padding the design books against -N is real only for a capture this
// code does not make, and the flag's absence can be asserted on the args and
// nowhere else. See TestCaptureRangeArgsCarryNoPaddingOrEscapes.
//
// -e stays off because its SGR sequences render as garbage in a <pre> and
// travel into whatever the clipboard is pasted into; leaving it off cost 54
// bytes out of 180 140 on a measured capture.
func captureRangeArgs(paneID string, lines int) []string {
	if lines < 1 {
		lines = 1
	}
	if lines > maxCaptureLines {
		lines = maxCaptureLines
	}
	return []string{"capture-pane", "-p", "-J", "-S", "-" + strconv.Itoa(lines), "-t", paneID}
}

// truncateHeadAtRuneBoundary keeps the LAST maxBytes of s, cutting forward to
// the start of a rune rather than back. Sibling of truncateAtRuneBoundary, and
// the direction is the whole point: see MaxCaptureBytes.
func truncateHeadAtRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// s[cut] is the first byte kept. If it does not begin a rune the cut falls
	// inside one, so walk forward over the rest of that rune -- the opposite
	// direction to truncateAtRuneBoundary, which walks back because the bytes
	// it is keeping are on the other side.
	cut := len(s) - maxBytes
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}
