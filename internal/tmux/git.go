package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxGitWalk bounds the climb from a pane's directory to the filesystem root.
//
// Measured on the owner's live server: maximum pane path depth 7. Forty is
// slack, and its job is a pathological path or a symlink loop, not a real tree.
const maxGitWalk = 40

// gitBranch is one full read of a directory's repository: the climb, the stat
// of HEAD and the parse of it. The entry it returns carries HEAD's identity
// beside the branch, so the reader's next pass over the same directory can stop
// at the stat -- see gitReader.check. checkedAt is the caller's to stamp.
//
// The branch is a branch name, or "@" + seven hex characters when HEAD is
// detached, or "" for everything else -- a bare repository, no repository at
// all, or a HEAD it cannot make sense of. No fork: two reads at most, and
// usually one.
//
// A zero entry is "no repository above dir", which is the answer the reader
// caches negatively.
//
// dir must be absolute. tmux reports a pane's cwd out of /proc and it always
// is, and a relative one would be walked from the daemon's own working
// directory -- filepath.Join("", ".git") is ".git" -- which is very often a
// checkout of something else entirely.
//
// fsys rather than os: see gitFS.
func gitBranch(fsys gitFS, dir string) gitEntry {
	if !filepath.IsAbs(dir) {
		return gitEntry{}
	}
	gitDir, ok := findGitDir(fsys, dir)
	if !ok {
		return gitEntry{}
	}
	e := gitEntry{gitDir: gitDir}
	// A HEAD that cannot be statted is left with a zero identity, which no
	// later stat can equal -- so the next pass reads it rather than believing
	// this one.
	if mtime, size, ok := statHead(fsys, gitDir); ok {
		e.headMtime, e.headSize = mtime, size
		e.branch = readHead(fsys, gitDir)
	}
	return e
}

// statHead is the cheap half of a pass: HEAD's mtime and size, which together
// are the only question a directory whose branch has not changed has to answer.
//
// Both halves, and not just the mtime. A checkout between two branches whose
// names are the same length is invisible to the size, and a write inside the
// filesystem's timestamp granularity is invisible to the mtime -- and git
// rewrites HEAD in place, so both are ordinary rather than exotic.
func statHead(fsys gitFS, gitDir string) (mtime time.Time, size int64, ok bool) {
	info, err := fsys.Stat(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return time.Time{}, 0, false
	}
	return info.ModTime(), info.Size(), true
}

// readHead is the expensive half, and the one a pass skips when the stat says
// nothing moved.
func readHead(fsys gitFS, gitDir string) string {
	head, err := fsys.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	return parseHead(string(head))
}

// findGitDir climbs from dir looking for .git, and resolves the `gitdir:`
// indirection a worktree or a submodule leaves in a .git FILE.
//
// The pointer is resolved against the directory holding the .git file and never
// against the pane's cwd: a submodule's is RELATIVE (`../../.git/modules/...`)
// and resolving it anywhere else names a directory that does not exist.
//
// A .git that is there but unusable ends the walk rather than continuing it. A
// broken submodule pointer must not be answered with its superproject's branch.
func findGitDir(fsys gitFS, dir string) (gitDir string, ok bool) {
	for i := 0; i < maxGitWalk; i++ {
		candidate := filepath.Join(dir, ".git")
		// Stat and not Lstat: a .git symlinked to a directory is still a git
		// directory, and a symlink loop comes back as an error and keeps the
		// climb going until maxGitWalk stops it.
		switch info, err := fsys.Stat(candidate); {
		case err == nil && info.IsDir():
			return candidate, true
		case err == nil:
			return resolveGitDirPointer(fsys, dir, candidate)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

// resolveGitDirPointer reads a `gitdir: <path>` line out of a .git file. base is
// the directory that file sits in, and a relative pointer is resolved against
// it.
func resolveGitDirPointer(fsys gitFS, base, file string) (gitDir string, ok bool) {
	contents, err := fsys.ReadFile(file)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(contents)), "gitdir:")
	if !ok {
		return "", false
	}
	if target = strings.TrimSpace(target); target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return target, true
}

// parseHead turns the contents of a HEAD file into what the row shows: a branch
// name, or "@" + the first seven characters of a detached hash, or "" for
// anything it does not recognise.
//
// Accepts 40 OR 64 hex characters. `git init --object-format=sha256` has
// existed since 2.29 and writes 64; a parser accepting only 40 fails closed in
// such a repo, silently, and looks like the feature not working.
//
// Everything else is refused, and that includes a short or torn line: git
// rewrites HEAD in place on checkout and a read can land mid-write. Showing
// nothing for one poll is the right answer there.
func parseHead(contents string) string {
	line := strings.TrimSpace(contents)
	// A HEAD file is one line. Anything with a second one is not a HEAD file.
	if strings.ContainsAny(line, "\r\n") {
		return ""
	}
	// The whole "refs/heads/" prefix goes, so a branch keeps its slashes:
	// "refs/heads/feat/x" is the branch "feat/x" and not "x". A HEAD pointing
	// anywhere but refs/heads is not a branch a row can name.
	if name, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
		return name
	}
	if isDetachedHash(line) {
		return "@" + line[:7]
	}
	return ""
}

// isDetachedHash reports whether line is a bare object name: exactly 40 hex
// characters (SHA-1) or exactly 64 (SHA-256), lower case, which is what git
// writes. Anything else is a torn read or not a hash at all.
func isDetachedHash(line string) bool {
	if len(line) != 40 && len(line) != 64 {
		return false
	}
	for i := 0; i < len(line); i++ {
		if c := line[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// gitFS is the disk the branch reader reaches through: the two calls it makes,
// and nothing else.
//
// It exists so that a test can wedge a stat and count a read. os.Stat and
// os.ReadFile take no context and cannot be interrupted, so the containment
// this whole file is arranged around -- a stat that never returns costs one
// branch and nothing else -- can only be shown against a filesystem that never
// returns. Nothing below reaches for os directly.
type gitFS interface {
	Stat(name string) (os.FileInfo, error)
	ReadFile(name string) ([]byte, error)
}

// osFS is the real one, and the only implementation outside a test.
type osFS struct{}

func (osFS) Stat(name string) (os.FileInfo, error) { return os.Stat(name) }

func (osFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// gitEntry is one directory's answer, and what it took to get it.
//
// gitDir "" is a MISS -- there is no repository above this directory -- and is
// the entry that checkedAt exists for. A hit carries HEAD's identity instead,
// so a pass that finds it unmoved can stop at the stat.
type gitEntry struct {
	gitDir    string
	branch    string
	headMtime time.Time
	headSize  int64
	checkedAt time.Time
}

// gitMissTTL is how long "there is no repository above this directory" is
// believed.
//
// The negative cache is the part that matters. Most panes are not in
// repositories, and re-walking eight levels every 1.5s for a shell sitting in
// $HOME is pure waste.
const gitMissTTL = 30 * time.Second

// gitReader keeps the branch of every directory the poll has seen, on a
// goroutine of its own.
//
// WHY IT IS NOT ON THE POLL. Every other read this daemon makes is an exec with
// a deadline. os.Stat has none, and a stat on a wedged NFS or sshfs mount
// blocks uninterruptibly -- so a git read inlined into refresh would spend the
// poll's whole budget, and the poll is the sidebar. Here, a hung filesystem
// costs a missing or a stale branch and nothing else.
//
// WHY IT IS KEYED BY DIRECTORY. Measured on the owner's live server: 12 panes,
// 3 distinct paths. Keyed by pane it would do four times the work for the same
// three answers -- and it would then need clearing when the tmux server
// restarts and renumbers the panes, which keyed by directory it does not.
//
// WHY EACH DIRECTORY IS ITS OWN UNIT OF WORK. One goroutine walking three paths
// in order stops at the first blocked stat and never refreshes the other two,
// which is the failure the separate goroutine was supposed to contain,
// reintroduced one level down. So each directory gets its own worker and its
// own in-flight flag, and a directory already being checked is SKIPPED rather
// than waited on: the wedged one stays wedged and the rest keep moving.
type gitReader struct {
	fs    gitFS
	nowFn func() time.Time

	mu       sync.Mutex
	entries  map[string]gitEntry
	inFlight map[string]struct{}
	// wanted is the mailbox: the newest set of directories, and only that one.
	// See want. waiting says there IS one, which is not the same question --
	// a poll whose panes are all pathless hands over an empty set, and a pass
	// over it must retain nothing, where a spurious wake must retain
	// everything.
	wanted  []string
	waiting bool
	// wake says a set is waiting. Capacity one and written to without
	// blocking, so it is a doorbell rather than a queue.
	wake chan struct{}
}

func newGitReader(fsys gitFS, nowFn func() time.Time) *gitReader {
	return &gitReader{
		fs:       fsys,
		nowFn:    nowFn,
		entries:  map[string]gitEntry{},
		inFlight: map[string]struct{}{},
		wake:     make(chan struct{}, 1),
	}
}

// want hands the reader the distinct directories of the poll that just
// finished. It never blocks, and it never queues: a set handed over while the
// reader is still busy OVERWRITES whatever was waiting.
//
// Dropping is the point. A queue turns one wedged stat into a growing backlog
// of sets that are stale before they are read, while the poller keeps producing
// another every 1.5s forever. The newest set is the only one worth having.
//
// dirs becomes the reader's; the poller builds a fresh slice per poll.
func (r *gitReader) want(dirs []string) {
	r.mu.Lock()
	r.wanted, r.waiting = dirs, true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// take empties the mailbox, and says whether there was anything in it. The
// doorbell can ring once more than there are sets -- want writes the set and
// rings separately -- and a pass over a set that was never handed over would
// retain nothing and empty the cache.
func (r *gitReader) take() (dirs []string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.waiting {
		return nil, false
	}
	dirs, r.wanted, r.waiting = r.wanted, nil, false
	return dirs, true
}

// branch is what a row shows for a pane sitting in dir: whatever the reader has
// already read, and "" for a directory it has not reached yet or cannot answer
// for. Never a wait -- that is the whole arrangement.
func (r *gitReader) branch(dir string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[dir].branch
}

// run is the reader's goroutine. It ends with ctx, and a wedged worker of its
// own outlives it: nothing can interrupt a stat, and pretending otherwise would
// mean holding the daemon's shutdown open on a mount that is never coming back.
func (r *gitReader) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		if dirs, ok := r.take(); ok {
			r.pass(dirs)
		}
	}
}

// pass starts a worker for every directory of one set that is not already being
// checked, and returns without waiting for any of them.
//
// Not waiting is what keeps the next set from queueing behind a wedged stat,
// and it is what makes the in-flight skip mean something: the pass after a
// wedge starts workers for every OTHER directory and leaves the wedged one
// alone.
func (r *gitReader) pass(dirs []string) {
	// One clock read for the whole pass, so that every directory in it is aged
	// against the same instant -- the same deal `now` gets in Poller.classify.
	now := r.nowFn()
	r.retain(dirs)
	for _, dir := range dirs {
		if !r.begin(dir) {
			continue
		}
		go func(dir string) {
			defer r.done(dir)
			r.check(dir, now)
		}(dir)
	}
}

// begin claims dir for a worker, and reports false if one already has it.
func (r *gitReader) begin(dir string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.inFlight[dir]; busy {
		return false
	}
	r.inFlight[dir] = struct{}{}
	return true
}

func (r *gitReader) done(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, dir)
}

// retain drops every directory no pane is in any more.
//
// The map is keyed by a path a person types, and a shell that has been cd-ed
// around for a month would otherwise leave an entry per directory it ever
// visited. Bounded to the panes that exist, it is one entry per distinct pane
// path -- three, on the server this was measured on.
func (r *gitReader) retain(dirs []string) {
	keep := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		keep[dir] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for dir := range r.entries {
		if _, ok := keep[dir]; !ok {
			delete(r.entries, dir)
		}
	}
}

// check is one directory's unit of work, and the only code here that touches
// the disk.
//
// Three shapes, in the order they cost: a known repository whose HEAD has not
// moved is one stat; a directory already known to hold no repository is
// nothing at all for gitMissTTL; anything else is a climb.
func (r *gitReader) check(dir string, now time.Time) {
	prev, known := r.entryFor(dir)
	switch {
	case known && prev.gitDir != "":
		mtime, size, ok := statHead(r.fs, prev.gitDir)
		if !ok {
			// The repository went away -- a removed worktree, a deleted
			// checkout. Forgetting it is what sends the next pass back up the
			// tree; keeping it would go on answering out of a cache that names
			// a directory nobody can stat.
			r.forget(dir)
			return
		}
		if mtime.Equal(prev.headMtime) && size == prev.headSize {
			return
		}
		next := prev
		next.branch = readHead(r.fs, prev.gitDir)
		next.headMtime, next.headSize, next.checkedAt = mtime, size, now
		r.store(dir, next)
	case known && now.Sub(prev.checkedAt) < gitMissTTL:
		// A miss still inside its TTL. Nothing is read, and that is the saving.
	default:
		e := gitBranch(r.fs, dir)
		e.checkedAt = now
		r.store(dir, e)
	}
}

func (r *gitReader) entryFor(dir string) (gitEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[dir]
	return e, ok
}

func (r *gitReader) store(dir string, e gitEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[dir] = e
}

func (r *gitReader) forget(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, dir)
}
