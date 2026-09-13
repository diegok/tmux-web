package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The branch reader is the one thing this daemon reads off the disk rather than
// out of tmux, and os.Stat takes no context. Everything below is about what
// that costs: it must cost a missing or stale branch on the one wedged
// directory, and nothing else -- not the poll, and not the other directories.

// --- the fixtures ------------------------------------------------------------

// repoAt builds a working tree at parent/name whose HEAD holds head, and
// returns its directory.
//
// Written by hand rather than by git, and deliberately: what git puts on disk
// is TestGitBranch's subject, and it is settled there against a real one. The
// subject here is the cache and the goroutine, which need a HEAD whose bytes
// and whose mtime the test controls to the byte and to the nanosecond.
func repoAt(t *testing.T, parent, name, head string) string {
	t.Helper()
	dir := mkdir(t, parent, name)
	writeFile(t, filepath.Join(mkdir(t, dir, ".git"), "HEAD"), head+"\n")
	return dir
}

// setHead rewrites a fixture's HEAD and stamps it with mtime, so that a test can
// say which HALF of the change detector it is exercising: same bytes with a new
// time, or new bytes at the old time.
func setHead(t *testing.T, dir, head string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, ".git", "HEAD")
	writeFile(t, path, head+"\n")
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func headMtime(t *testing.T, dir string) time.Time {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("stat HEAD in %s: %v", dir, err)
	}
	return info.ModTime()
}

// testFS is the disk the reader tests reach through: it counts, and it wedges.
//
// A stat that returns instantly cannot show that a stat that never returns is
// contained, so the wedge is the instrument the whole file is built on. A
// wedged path blocks in Stat until the test releases it -- which most of these
// tests never do -- and inStat says how many callers are parked there right
// now, so a test can assert the block was real at the moment it made its claim.
type testFS struct {
	mu      sync.Mutex
	stats   map[string]int
	reads   map[string]int
	wedged  []string
	inStat  int
	entered chan string

	release chan struct{}
}

func newTestFS() *testFS {
	return &testFS{
		stats:   map[string]int{},
		reads:   map[string]int{},
		entered: make(chan string, 64),
		release: make(chan struct{}),
	}
}

// wedge makes every stat of anything under dir block until releaseAll.
func (f *testFS) wedge(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wedged = append(f.wedged, dir)
}

func (f *testFS) releaseAll() { close(f.release) }

func (f *testFS) Stat(name string) (os.FileInfo, error) {
	f.mu.Lock()
	f.stats[name]++
	blocked := false
	for _, w := range f.wedged {
		if name == w || strings.HasPrefix(name, w+string(os.PathSeparator)) {
			blocked = true
		}
	}
	if blocked {
		f.inStat++
	}
	f.mu.Unlock()
	if blocked {
		select {
		case f.entered <- name:
		default:
		}
		<-f.release
		f.mu.Lock()
		f.inStat--
		f.mu.Unlock()
	}
	return os.Stat(name)
}

func (f *testFS) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	f.reads[name]++
	f.mu.Unlock()
	return os.ReadFile(name)
}

// heads counts stats, or reads, of a HEAD file -- the reader's unit of work.
func (f *testFS) heads(counts func(*testFS) map[string]int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for path, c := range counts(f) {
		if filepath.Base(path) == "HEAD" {
			n += c
		}
	}
	return n
}

func (f *testFS) headStats() int { return f.heads(func(f *testFS) map[string]int { return f.stats }) }
func (f *testFS) headReads() int { return f.heads(func(f *testFS) map[string]int { return f.reads }) }

// walks counts stats of a .git candidate: one per level of one climb.
func (f *testFS) walks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for path, c := range f.stats {
		if filepath.Base(path) == ".git" {
			n += c
		}
	}
	return n
}

func (f *testFS) parked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inStat
}

// awaitWedge blocks until a stat has actually parked, so that a test claiming
// "while the reader is blocked" is saying something true rather than hoping.
func (f *testFS) awaitWedge(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(testDeadline):
		t.Fatal("no stat ever reached the wedged directory: this test proves nothing without one")
	}
}

// testDeadline is how long any of these tests waits for the reader's goroutine
// before calling it a failure. Generous, because it is only ever paid on the
// way to a t.Fatal.
const testDeadline = 5 * time.Second

// awaitBranch waits for the reader to have read dir's branch. Every read goes
// through the reader's own mutex, so this is synchronisation and not a spin on
// a shared variable; the sleep is backoff, not the barrier.
func awaitBranch(t *testing.T, r *gitReader, dir, want string) {
	t.Helper()
	for deadline := time.Now().Add(testDeadline); time.Now().Before(deadline); {
		if got := r.branch(dir); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("branch(%s) = %q after %v, want %q", filepath.Base(dir), r.branch(dir), testDeadline, want)
}

// mustReturn runs fn and fails -- with a sentence, not a hung binary -- if it
// has not come back within the deadline. Every "this must not block" assertion
// in this file goes through it: a test that hangs instead of failing has killed
// no mutant, it has only stopped the suite.
func mustReturn(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(testDeadline):
		t.Fatalf("%s did not return within %v: the poll goroutine is parked on something that never answers", what, testDeadline)
	}
}

// startReader builds a reader over fs and runs its goroutine for the test.
func startReader(t *testing.T, fs gitFS, nowFn func() time.Time) *gitReader {
	t.Helper()
	r := newGitReader(fs, nowFn)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.run(ctx)
	return r
}

// --- the headline claim ------------------------------------------------------

// The whole reason the reader is a goroutine: a stat that never returns must
// cost a branch, never a poll.
//
// The poll runs under one deadline (see TestTheWholePollRunsUnderOneDeadline)
// and every other read it makes is an exec with a context. os.Stat has neither,
// and a stat on a wedged NFS or sshfs mount is uninterruptible -- so a git read
// inlined into refresh parks the poll goroutine for as long as the mount is
// wedged, every tick is dropped, and the sidebar freezes while the daemon goes
// on reporting itself fresh.
func TestGitReaderDoesNotRunOnThePollGoroutine(t *testing.T) {
	dir := repoAt(t, t.TempDir(), "wedged", "ref: refs/heads/main")
	fs := newTestFS()
	fs.wedge(dir)

	p := NewPollerWith(Options{
		Interval: time.Hour,
		Snapshot: func(context.Context) ([]Row, error) {
			return []Row{{PaneID: "%0", Path: dir}}, nil
		},
		Branches: true,
	})
	p.git = startReader(t, fs, p.nowFn)

	// The first poll is the one an inlined read would park: it is the poll that
	// first sees the directory.
	mustReturn(t, "the first poll", func() { p.refresh(context.Background()) })
	fs.awaitWedge(t)
	// And the second, with the reader now demonstrably stuck.
	mustReturn(t, "the second poll", func() { p.refresh(context.Background()) })

	if got := fs.parked(); got != 1 {
		t.Fatalf("%d stats are parked in the wedged directory, want 1: the fixture answered, so this "+
			"test would pass over an inlined read as happily as over the goroutine", got)
	}
	if len(p.Latest()) != 1 || p.Latest()[0].Branch != "" {
		t.Errorf("the wedged pane's row is %+v, want an empty branch: nothing was ever read for it", p.Latest())
	}
}

// A serial walker survives the test above -- it blocks the reader's goroutine
// and not the poll -- and empties the whole sidebar's branches on one wedged
// mount, which is the failure the goroutine was supposed to contain,
// reintroduced one level down. Each directory is its own unit of work.
//
// The wedged directory is FIRST in the set, because a serial loop that happens
// to reach it last would refresh the other two before parking.
func TestOneWedgedDirectoryDoesNotStallTheOthers(t *testing.T) {
	root := t.TempDir()
	wedged := repoAt(t, root, "wedged", "ref: refs/heads/wedged")
	b := repoAt(t, root, "b", "ref: refs/heads/bee")
	c := repoAt(t, root, "c", "ref: refs/heads/cee")
	fs := newTestFS()
	fs.wedge(wedged)
	r := startReader(t, fs, time.Now)

	r.want([]string{wedged, b, c})

	awaitBranch(t, r, b, "bee")
	awaitBranch(t, r, c, "cee")
	fs.awaitWedge(t)

	// And a second pass arriving while the first is still parked. A reader that
	// WAITS on a directory already in flight rather than skipping it gets no
	// further than the wedged one here, and b and c keep the branches they had
	// -- which the first half of this test has already established.
	setHead(t, b, "ref: refs/heads/bee2", time.Now())
	setHead(t, c, "ref: refs/heads/cee2", time.Now())
	r.want([]string{wedged, b, c})
	awaitBranch(t, r, b, "bee2")
	awaitBranch(t, r, c, "cee2")

	if got := fs.parked(); got != 1 {
		t.Errorf("%d stats are parked, want exactly 1: the wedge must still be holding, and the second "+
			"pass must not have started a second worker on a directory already in flight", got)
	}
	if r.branch(wedged) != "" {
		t.Errorf("the wedged directory reported %q; nothing has ever come back for it", r.branch(wedged))
	}
}

// The handoff drops path sets; it never queues them.
//
// A queue turns one wedged stat into a growing backlog of sets that are stale
// before they are read, while the poller keeps producing another every
// interval, forever. The newest set is the only one worth having.
//
// The reader is made busy through its clock, which it reads once per pass: that
// is the one place a pass is guaranteed to pause, and blocking it needs no seam
// that exists only for this test.
func TestTheMailboxHoldsOneSetAndDropsTheRest(t *testing.T) {
	root := t.TempDir()
	first := repoAt(t, root, "first", "ref: refs/heads/one")
	second := repoAt(t, root, "second", "ref: refs/heads/two")
	third := repoAt(t, root, "third", "ref: refs/heads/three")

	var mu sync.Mutex
	var passes int
	busy := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	// So that a failure below leaves nothing parked in the clock.
	t.Cleanup(release)
	nowFn := func() time.Time {
		mu.Lock()
		passes++
		n := passes
		mu.Unlock()
		if n == 1 {
			close(busy)
			<-resume
		}
		return time.Now()
	}
	r := startReader(t, newTestFS(), nowFn)

	r.want([]string{first})
	select {
	case <-busy: // the reader has taken the first set and is inside it
	case <-time.After(testDeadline):
		t.Fatal("the reader never started a pass over the first set")
	}

	// Both offers under a deadline: a mailbox whose send BLOCKS would park the
	// poll goroutine here, and a test that hangs has killed nothing.
	mustReturn(t, "handing the reader a second and a third set", func() {
		r.want([]string{second})
		r.want([]string{third})
	})
	release()

	awaitBranch(t, r, first, "one")
	awaitBranch(t, r, third, "three")
	if got := r.branch(second); got != "" {
		t.Errorf("the reader read the second set too (%s = %q): the mailbox queued instead of dropping, "+
			"and under a wedged filesystem that backlog grows forever", filepath.Base(second), got)
	}
	mu.Lock()
	defer mu.Unlock()
	if passes != 2 {
		t.Errorf("the reader ran %d passes over three sets, want 2: the first, and the newest one waiting", passes)
	}
}

// --- the cache ---------------------------------------------------------------

// One stat per directory per pass, and a read only when HEAD has moved. The
// poll is 1.5s; re-reading three unchanged files forty times a minute forever
// is the cost this exists to not pay.
func TestAnUnchangedHeadIsNotReRead(t *testing.T) {
	dir := repoAt(t, t.TempDir(), "repo", "ref: refs/heads/main")
	fs := newTestFS()
	r := startReader(t, fs, time.Now)

	r.want([]string{dir})
	awaitBranch(t, r, dir, "main")
	if got := fs.headReads(); got != 1 {
		t.Fatalf("the first pass read HEAD %d times, want 1", got)
	}

	r.want([]string{dir})
	// The second pass is not observable through the branch -- it does not
	// change -- so it is awaited through the stat it must still make.
	awaitStats(t, fs, 2)
	if got := fs.headReads(); got != 1 {
		t.Errorf("two passes over an untouched repository read HEAD %d times, want 1", got)
	}

	t.Run("a moved HEAD is re-read", func(t *testing.T) {
		setHead(t, dir, "ref: refs/heads/other", time.Now())
		r.want([]string{dir})
		awaitBranch(t, r, dir, "other")
		if got := fs.headReads(); got != 2 {
			t.Errorf("HEAD was read %d times over three passes, want 2: one for the first sight and one for the move", got)
		}
	})

	// The two halves of the change detector, one row each. Both are forced
	// rather than raced: os.Chtimes puts the mtime exactly where the row needs
	// it, so neither row depends on this filesystem's timestamp granularity.
	t.Run("same length, new mtime", func(t *testing.T) {
		before := fs.headReads()
		was := headMtime(t, dir)
		// "other" -> "OTHER" is the same size, so only the mtime differs.
		setHead(t, dir, "ref: refs/heads/OTHER", was.Add(time.Second))
		r.want([]string{dir})
		awaitBranch(t, r, dir, "OTHER")
		if got := fs.headReads(); got != before+1 {
			t.Errorf("a same-length branch at a new mtime was read %d times, want 1: a detector comparing "+
				"only the size never sees a checkout between two branches whose names are the same length",
				got-before)
		}
	})

	t.Run("same mtime, new length", func(t *testing.T) {
		before := fs.headReads()
		was := headMtime(t, dir)
		setHead(t, dir, "ref: refs/heads/a-considerably-longer-branch", was)
		r.want([]string{dir})
		awaitBranch(t, r, dir, "a-considerably-longer-branch")
		if got := fs.headReads(); got != before+1 {
			t.Errorf("a longer branch at the same mtime was read %d times, want 1: a detector comparing "+
				"only the mtime is blind to a write inside its granularity", got-before)
		}
	})
}

// awaitStats waits until HEAD has been statted n times.
func awaitStats(t *testing.T, fs *testFS, n int) {
	t.Helper()
	for deadline := time.Now().Add(testDeadline); time.Now().Before(deadline); {
		if fs.headStats() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("HEAD was statted %d times after %v, want %d", fs.headStats(), testDeadline, n)
}

// Most panes are not in a repository at all, and re-climbing eight levels every
// 1.5s for a shell sitting in $HOME is pure waste. The negative cache is the
// part of this feature that pays for itself.
func TestAMissIsCachedForGitMissTTL(t *testing.T) {
	root := t.TempDir()
	requireNoRepoAbove(t, root)
	dir := mkdirDepth(t, root, "deep", 3)

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := base
	fs := newTestFS()
	r := startReader(t, fs, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	})

	r.want([]string{dir})
	// The whole climb, not a count taken in the middle of one.
	awaitEntry(t, r, dir)
	first := fs.walks()
	if first < 2 {
		t.Fatalf("the first pass statted %d .git candidates, want a climb of several: the fixture is not "+
			"deep enough for a re-walk to be visible", first)
	}

	// A second pass on the same clock, and a third: neither may climb again.
	r.want([]string{dir})
	r.want([]string{dir})
	awaitQuiet(t, fs, first)
	if got := fs.walks(); got != first {
		t.Errorf("two more passes climbed %d more times, want 0: with no negative cache a shell in a "+
			"home directory re-walks its whole path forty times a minute", got-first)
	}

	// 31s, spelled out. A TTL retargeted to zero must fail this row, and it
	// cannot if the row's own advance is computed from the constant it tests.
	mu.Lock()
	clock = base.Add(31 * time.Second)
	mu.Unlock()
	r.want([]string{dir})
	awaitWalks(t, fs, first+1)
}

// awaitEntry waits until the reader has an answer of its own for dir --
// including a negative one, which no accessor of the reader's can show and
// which is the whole subject of the test above.
func awaitEntry(t *testing.T, r *gitReader, dir string) {
	t.Helper()
	for deadline := time.Now().Add(testDeadline); time.Now().Before(deadline); {
		r.mu.Lock()
		_, ok := r.entries[dir]
		r.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the reader reached no verdict on %s within %v", filepath.Base(dir), testDeadline)
}

func awaitWalks(t *testing.T, fs *testFS, n int) {
	t.Helper()
	for deadline := time.Now().Add(testDeadline); time.Now().Before(deadline); {
		if fs.walks() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the reader climbed %d times after %v, want at least %d", fs.walks(), testDeadline, n)
}

// awaitQuiet gives the reader room to do the wrong thing before the test says it
// did not: two passes are handed over, and this waits for both to have been
// taken and finished.
func awaitQuiet(t *testing.T, fs *testFS, was int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fs.walks() > was {
			return // the assertion that follows is about to fail, and should
		}
		time.Sleep(time.Millisecond)
	}
}

// --- the ratio ---------------------------------------------------------------

// Measured on the owner's live server: 12 panes, 3 distinct paths. The cache is
// keyed by DIRECTORY, and that ratio is the whole reason -- a cache keyed by
// pane id does four times the work for the same three answers, and every other
// test in this file passes over it.
func TestTwelvePanesInThreeDirectoriesAreThreeUnitsOfWork(t *testing.T) {
	root := t.TempDir()
	dirs := []string{
		repoAt(t, root, "one", "ref: refs/heads/one"),
		repoAt(t, root, "two", "ref: refs/heads/two"),
		repoAt(t, root, "three", "ref: refs/heads/three"),
	}
	var rows []Row
	for i := 0; i < 12; i++ {
		rows = append(rows, Row{PaneID: "%" + string(rune('a'+i)), Path: dirs[i%3]})
	}

	fs := newTestFS()
	p := NewPollerWith(Options{
		Interval: time.Hour,
		Snapshot: func(context.Context) ([]Row, error) {
			out := make([]Row, len(rows))
			copy(out, rows)
			return out, nil
		},
		Branches: true,
	})
	p.git = startReader(t, fs, p.nowFn)

	p.refresh(context.Background())
	for _, dir := range dirs {
		awaitBranch(t, p.git, dir, filepath.Base(dir))
	}
	if got := fs.headStats(); got != 3 {
		t.Errorf("twelve panes in three directories statted HEAD %d times, want 3", got)
	}

	// And the second poll is the one that carries them: the rows are filled
	// from what the reader has ALREADY read, which is what keeps this call off
	// the filesystem.
	p.refresh(context.Background())
	for _, row := range p.Latest() {
		if want := filepath.Base(row.Path); row.Branch != want {
			t.Errorf("pane %s in %s shows branch %q, want %q", row.PaneID, want, row.Branch, want)
		}
	}
}

// The git reader must add ZERO forks: it reads files.
//
// TestOnePollForksTmuxOnce is the claim this one extends, and the shim is the
// only instrument that can see the difference -- a branch read through `git
// rev-parse`, or a working directory fetched with a second display-message,
// would agree with the files on every assertion and cost a fork per pane per
// poll forever.
func TestOnePollStillForksTmuxOnce(t *testing.T) {
	root := t.TempDir()
	dirs := []string{
		repoAt(t, root, "one", "ref: refs/heads/one"),
		repoAt(t, root, "two", "ref: refs/heads/two"),
		repoAt(t, root, "three", "ref: refs/heads/three"),
	}
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", "-c", dirs[0])
	srv.Run(t, "new-window", "-d", "-t", "work", "-c", dirs[1])
	srv.Run(t, "new-window", "-d", "-t", "work", "-c", dirs[2])

	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux not found: %v", err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "invocations")
	shim := "#!/bin/sh\necho x >> " + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	// After the fixture is seeded, so only the poll is counted.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := NewClient(srv.Args())
	p := NewPollerWith(Options{
		Interval:  time.Hour,
		Poll:      c.Poll,
		Capture:   c.Capture,
		Connected: func() bool { return false },
		Branches:  true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.git.run(ctx)

	p.refresh(context.Background())

	if err := p.Err(); err != nil {
		t.Fatalf("the poll failed, so this counted nothing: %v", err)
	}
	if len(p.Latest()) != 3 {
		t.Fatalf("the poll did not land: rows %+v", p.Latest())
	}
	// The reader really did its work, so the fork count below is a count taken
	// over a working feature rather than over one that did nothing.
	for _, dir := range dirs {
		awaitBranch(t, p.git, dir, filepath.Base(dir))
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the shim was never run, so PATH did not reach the client: %v", err)
	}
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Errorf("one poll with the branch reader wired forked tmux %d times, want 1: the branch is read "+
			"off the disk, and a fork per pane per poll is exactly what that avoids", got)
	}
}

// The map is keyed by a path a person types, so it is pruned to the panes that
// exist. A shell cd-ed around for a month would otherwise leave an entry per
// directory it ever visited, and nothing else ever drops one: the cache
// survives a tmux server restart by design.
func TestADirectoryNoPaneIsInAnyMoreIsForgotten(t *testing.T) {
	root := t.TempDir()
	stays := repoAt(t, root, "stays", "ref: refs/heads/stays")
	goes := repoAt(t, root, "goes", "ref: refs/heads/goes")
	r := startReader(t, newTestFS(), time.Now)

	r.want([]string{stays, goes})
	awaitBranch(t, r, stays, "stays")
	awaitBranch(t, r, goes, "goes")

	r.want([]string{stays})
	awaitBranch(t, r, goes, "")
	if got := r.branch(stays); got != "stays" {
		t.Errorf("the directory still in the set reports %q, want %q: the prune took the wrong entries", got, "stays")
	}
}

// A directory whose repository has gone -- `git worktree remove`, a deleted
// checkout -- must lose its branch and be climbed again, not keep answering out
// of a cache that names a directory nobody can stat.
func TestAVanishedRepositoryIsForgotten(t *testing.T) {
	root := t.TempDir()
	requireNoRepoAbove(t, root)
	dir := repoAt(t, root, "repo", "ref: refs/heads/main")
	fs := newTestFS()
	r := startReader(t, fs, time.Now)

	r.want([]string{dir})
	awaitBranch(t, r, dir, "main")

	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}
	r.want([]string{dir})
	awaitBranch(t, r, dir, "")
}

// The poll's own bookkeeping, which is one line of it: the distinct
// directories, once each, and nothing for a pane whose path the snapshot did
// not carry.
func TestAPaneWithNoPathAsksForNothing(t *testing.T) {
	fs := newTestFS()
	p := NewPollerWith(Options{
		Interval: time.Hour,
		Snapshot: func(context.Context) ([]Row, error) {
			return []Row{{PaneID: "%0"}, {PaneID: "%1"}}, nil
		},
		Branches: true,
	})
	p.git = startReader(t, fs, p.nowFn)

	p.refresh(context.Background())
	awaitQuiet(t, fs, 0)
	if got := fs.walks(); got != 0 {
		t.Errorf("a poll of two pathless rows climbed %d times: \"\" is not a directory, and walking it "+
			"from the daemon's own working directory would name a checkout of something else entirely", got)
	}
	if got := p.Latest()[0].Branch; got != "" {
		t.Errorf("a pathless row shows branch %q", got)
	}
}
