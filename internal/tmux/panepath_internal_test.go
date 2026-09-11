package tmux

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// panePath used to fork tmux a second time on every split and every new
// window, to re-read a value the poller already holds. These pin what replaced
// it: the cached path is a hint, and the stat is still the gate.

// cacheFixture is a server with one pane sitting in a directory of its own, and
// a client whose path cache the test controls.
type cacheFixture struct {
	srv  *testutil.Server
	c    *Client
	pane string
	// live is where the pane really is, which is what a live read answers.
	live string
	// asked records the pane ids the cache was consulted for.
	asked []string
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	srv := testutil.NewServer(t)
	f := &cacheFixture{srv: srv, live: mkdir(t, t.TempDir(), "live")}
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", "-c", f.live)
	f.pane = srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_id}")
	f.c = NewClient(srv.Args())
	return f
}

// cache points the client at a fixed answer and records that it was asked.
func (f *cacheFixture) cache(dir string, ok bool) {
	f.c.UsePathCache(func(paneID string) (string, bool) {
		f.asked = append(f.asked, paneID)
		return dir, ok
	})
}

// dirOf is where tmux says a pane is, read back through the server directly so
// that the assertion does not go through the code under test.
func (f *cacheFixture) dirOf(t *testing.T, paneID string) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		got := f.srv.Run(t, "list-panes", "-t", paneID, "-f", "#{==:#{pane_id},"+paneID+"}",
			"-F", "#{pane_current_path}")
		if got != "" {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pane %s never reported a working directory", paneID)
	return ""
}

// The payoff: the poller already read this value, so the split does not fork
// tmux again to read it. The cached directory is deliberately NOT where the
// pane really is, because "the new pane landed in the right place" is also true
// of a split that ignored the cache and re-read the pane -- and that assertion
// would not be able to tell the two apart.
func TestSplitPaneUsesTheCachedPath(t *testing.T) {
	f := newCacheFixture(t)
	cached := mkdir(t, t.TempDir(), "cached")
	f.cache(cached, true)

	id, err := f.c.SplitPane(context.Background(), f.pane, SplitRight)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if got := f.dirOf(t, id); got != resolved(t, cached) {
		t.Errorf("split opened in %q, want the cached %q", got, resolved(t, cached))
	}
	if len(f.asked) == 0 || f.asked[0] != f.pane {
		t.Errorf("the cache was asked for %q, want one question about %q", f.asked, f.pane)
	}
}

func TestNewWindowUsesTheCachedPath(t *testing.T) {
	f := newCacheFixture(t)
	cached := mkdir(t, t.TempDir(), "cached")
	f.cache(cached, true)

	id, err := f.c.NewWindow(context.Background(), "$0", "", f.pane)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	pane := f.srv.Run(t, "list-panes", "-t", id, "-F", "#{pane_id}")
	if got := f.dirOf(t, pane); got != resolved(t, cached) {
		t.Errorf("the new window opened in %q, want the cached %q", got, resolved(t, cached))
	}
	if len(f.asked) == 0 {
		t.Error("the cache was never asked; the window forked tmux to re-read the path")
	}
}

// The caveat that comes with the cache, and it is not optional: a cached path
// is up to a poll interval old and can name a directory that has since been
// removed. `split-window -c /gone` exits 0 and starts the shell in $HOME
// instead -- measured -- so without the stat the owner gets a pane in the wrong
// place and no indication of it. This saves the fork, not the check.
func TestACachedPathThatIsGoneIsNotUsed(t *testing.T) {
	f := newCacheFixture(t)
	gone := mkdir(t, t.TempDir(), "gone")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	f.cache(gone, true)

	id, err := f.c.SplitPane(context.Background(), f.pane, SplitRight)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	// Where the pane really is, read live -- and NOT $HOME, which is where an
	// unchecked `-c` lands.
	if got := f.dirOf(t, id); got != resolved(t, f.live) {
		t.Errorf("split opened in %q, want the pane's live %q: a cached directory "+
			"that is gone must not reach `-c`", got, resolved(t, f.live))
	}
}

// A cache that has never seen this pane -- a pane created since the last poll,
// or a daemon polling without the path block -- is not an error. The live read
// is still there.
func TestAnAbsentCacheEntryFallsBackToTheLiveRead(t *testing.T) {
	f := newCacheFixture(t)
	f.cache("", false)

	id, err := f.c.SplitPane(context.Background(), f.pane, SplitRight)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if got := f.dirOf(t, id); got != resolved(t, f.live) {
		t.Errorf("split opened in %q, want the pane's live %q", got, resolved(t, f.live))
	}
}

// The id is validated before anything is looked up. A cache keyed on an
// unvalidated id is a cache that can be asked about "" -- and "" is the id tmux
// resolves to "whatever is current".
//
// Driven through NewWindow, and that is not a detail: SplitPane validates the
// same id itself before it ever calls panePath, so a split can never reach the
// lookup with a bad id and a test written on one asserts nothing about the
// order inside panePath -- it passes with the validation deleted. NewWindow's
// fromPane is the parameter that arrives unchecked, straight from the browser's
// JSON body.
func TestAnInvalidPaneIDNeverReachesTheCache(t *testing.T) {
	f := newCacheFixture(t)
	f.cache(mkdir(t, t.TempDir(), "cached"), true)

	before := f.srv.Run(t, "list-windows", "-t", "work", "-F", "#{window_id}")
	if _, err := f.c.NewWindow(context.Background(), "$0", "", "nonsense"); err == nil {
		t.Fatal("NewWindow from an unparseable pane id = nil, want an error")
	}
	if len(f.asked) != 0 {
		t.Errorf("the cache was asked about %q before the id was validated", f.asked)
	}
	// A cached directory exists, so a lookup that happened first would have
	// answered and the window would have been created in it.
	if after := f.srv.Run(t, "list-windows", "-t", "work", "-F", "#{window_id}"); after != before {
		t.Errorf("NewWindow created a window anyway: %q -> %q", before, after)
	}
}

// --- the cache itself -------------------------------------------------------

// What the poller serves is the path from the most recent snapshot, and only
// when it has one: "" is not a directory, and handing it to `-c` would be
// handing tmux "start wherever you like".
func TestPollerPathFor(t *testing.T) {
	p := NewPollerFunc(time.Hour, func(context.Context) ([]Row, error) {
		return []Row{
			{PaneID: "%1", Path: "/tmp/one"},
			{PaneID: "%2", Path: ""},
		}, nil
	})

	if _, ok := p.PathFor("%1"); ok {
		t.Error("PathFor answered before the first poll")
	}
	p.refresh(context.Background())

	if got, ok := p.PathFor("%1"); !ok || got != "/tmp/one" {
		t.Errorf("PathFor(%%1) = %q, %v; want /tmp/one, true", got, ok)
	}
	if got, ok := p.PathFor("%2"); ok {
		t.Errorf("PathFor(%%2) = %q, %v; want a miss: a pane with no path has none to give", got, ok)
	}
	if got, ok := p.PathFor("%9"); ok {
		t.Errorf("PathFor(%%9) = %q, %v; want a miss for a pane the poll never saw", got, ok)
	}
}

// The two halves, wired as the daemon wires them: what the poller saw is what
// the split uses.
func TestAPollerFedCacheCarriesThePathToASplit(t *testing.T) {
	srv := testutil.NewServer(t)
	dir := mkdir(t, t.TempDir(), "polled-notes")
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", "-c", dir)
	pane := srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_id}")

	c := NewClient(srv.Args())
	p := NewPollerWith(Options{Interval: time.Hour, SnapshotWithReports: c.SnapshotAndReports})
	p.refresh(context.Background())
	c.UsePathCache(p.PathFor)

	got, ok := p.PathFor(pane)
	if !ok || got != resolved(t, dir) {
		t.Fatalf("the poller holds %q, %v for pane %s; want %q", got, ok, pane, resolved(t, dir))
	}

	id, err := c.SplitPane(context.Background(), pane, SplitDown)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	live := srv.Run(t, "list-panes", "-t", id, "-f", "#{==:#{pane_id},"+id+"}",
		"-F", "#{pane_current_path}")
	if live != resolved(t, dir) {
		t.Errorf("split opened in %q, want %q", live, resolved(t, dir))
	}
	// The directory the fixture chose has "n"s in it for the same reason the
	// snapshot fixtures do; a path that lost them would not exist and the split
	// would have fallen back to a live read instead of failing.
	if !strings.Contains(got, "notes") {
		t.Errorf("the polled path %q lost characters on the way", got)
	}
}
