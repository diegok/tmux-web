package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Built by git rather than written by hand, deliberately. The worktree and the
// submodule are the two cases the "one file" claim is wrong about, and a
// fixture written from the claim would encode the claim rather than test it.
// Skip the whole test if git is not on PATH -- t.Skip, not t.Fatal: this is the
// one test in the package whose subject is another program's on-disk format.
func TestGitBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	root := t.TempDir()
	// Every "not a repo" row below asserts "" by climbing to the filesystem
	// root. If TMPDIR itself sat inside a working tree those rows would find
	// that repo and pass for the wrong reason -- a fixture true by accident.
	requireNoRepoAbove(t, root)

	// A plain checkout on a one-word branch, and the same repo re-used as the
	// host for the two walk-depth rows and the child-directory row.
	normal := mkdir(t, root, "normal")
	git(t, normal, "init", "-q", "-b", "main", ".")
	commit(t, normal, "a")

	// A branch with a slash in it. "refs/heads/" is stripped by prefix and not
	// by "last path element", which this row is what pins.
	slashed := mkdir(t, root, "slashed")
	git(t, slashed, "init", "-q", "-b", "main", ".")
	commit(t, slashed, "a")
	git(t, slashed, "checkout", "-q", "-b", "feat/x")

	// Detached in a SHA-1 repository: a 40-hex line.
	detached1 := mkdir(t, root, "detached-sha1")
	git(t, detached1, "init", "-q", "-b", "main", ".")
	commit(t, detached1, "a")
	git(t, detached1, "checkout", "-q", "--detach")
	head1 := git(t, detached1, "rev-parse", "HEAD")
	if len(head1) != 40 {
		t.Fatalf("sha-1 rev-parse HEAD = %q, want 40 hex characters", head1)
	}

	// Detached in a SHA-256 repository: a 64-hex line. This is the headline
	// row: a parser that accepts only 40 hex shows nothing here, fails closed
	// and silently, and looks like the branch feature not working at all.
	detached256 := mkdir(t, root, "detached-sha256")
	git(t, detached256, "init", "-q", "-b", "main", "--object-format=sha256", ".")
	commit(t, detached256, "a")
	git(t, detached256, "checkout", "-q", "--detach")
	head256 := git(t, detached256, "rev-parse", "HEAD")
	if len(head256) != 64 {
		t.Fatalf("sha-256 rev-parse HEAD = %q, want 64 hex characters", head256)
	}

	// Unborn. Indistinguishable from a normal checkout by the file's contents
	// alone, and it shows the branch name, which is what `git branch
	// --show-current` says too. Recorded so nobody "fixes" it with a refs read.
	unborn := mkdir(t, root, "unborn")
	git(t, unborn, "init", "-q", "-b", "master", ".")

	// A worktree leaves an ABSOLUTE `gitdir:` pointer in a .git FILE.
	wtHost := mkdir(t, root, "worktree-host")
	git(t, wtHost, "init", "-q", "-b", "main", ".")
	commit(t, wtHost, "a")
	worktree := filepath.Join(root, "worktree")
	git(t, wtHost, "worktree", "add", "-q", "-b", "wtbranch", worktree)
	requireGitIsAFile(t, worktree)

	// A submodule leaves a RELATIVE `gitdir:` pointer in a .git FILE, and it is
	// relative to the directory holding that file. `protocol.file.allow` is
	// needed because the "remote" here is a local path.
	subOrigin := mkdir(t, root, "sub-origin")
	git(t, subOrigin, "init", "-q", "-b", "subbranch", ".")
	commit(t, subOrigin, "s")
	super := mkdir(t, root, "super")
	git(t, super, "init", "-q", "-b", "main", ".")
	commit(t, super, "x")
	git(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "-b", "subbranch", subOrigin, "vendor/sub")
	submodule := filepath.Join(super, "vendor", "sub")
	requireGitIsAFile(t, submodule)
	// A directory INSIDE the submodule, so that the pane's cwd and the
	// directory holding the .git file are different. Resolving the relative
	// pointer against the pane's cwd names a directory that does not exist,
	// and only this row can see the difference -- from the submodule's own
	// root the two bases are the same path and the mutant survives.
	submoduleNested := mkdir(t, submodule, "nested")

	// A bare repository has HEAD at its root and no .git anywhere. Not
	// supported: there is no working tree to be on a branch in.
	bare := mkdir(t, root, "bare.git")
	git(t, bare, "init", "-q", "-b", "main", "--bare", ".")

	// Not a repository, up to the filesystem root.
	plain := mkdir(t, root, "plain")

	// A child directory of a repository: the walk finds it one level up.
	child := mkdir(t, normal, "child")

	// Twenty levels below the repository root, a literal 20, so the repository
	// is the twenty-first directory the walk examines. Lowering maxGitWalk
	// makes this row stop short and show nothing.
	deepFound := mkdirDepth(t, normal, "d", 20)

	// Forty-one levels below the repository root, a literal 41 and never
	// maxGitWalk+1: derived from the constant, fixture and assertion would
	// move together and a retargeted bound would survive. Raising maxGitWalk
	// makes this row reach the repository and show a branch.
	deepGivenUp := mkdirDepth(t, normal, "e", 41)

	// .git exists as a directory but HEAD is not readable. Fails closed.
	noHead := mkdir(t, root, "no-head")
	mkdir(t, noHead, ".git")

	// .git is a file whose pointer names a directory that is not there. Fails
	// closed rather than climbing past it into the enclosing repository -- a
	// broken submodule must not report its superproject's branch.
	brokenHost := mkdir(t, root, "broken-host")
	git(t, brokenHost, "init", "-q", "-b", "main", ".")
	commit(t, brokenHost, "a")
	brokenPointer := mkdir(t, brokenHost, "broken")
	writeFile(t, filepath.Join(brokenPointer, ".git"), "gitdir: ../nowhere/at/all\n")

	// .git is a file that is not a gitdir pointer at all. Same rule, and it is
	// this row and not the one above that pins it: a pointer to a missing
	// directory still parses, so the walk stops either way and only a file the
	// parser rejects can tell "stop here" from "keep climbing".
	garbagePointer := mkdir(t, brokenHost, "garbage")
	writeFile(t, filepath.Join(garbagePointer, ".git"), "this is not a gitdir pointer\n")

	// The two literal depths above bracket the constant. Without this line the
	// depth rows say nothing about what maxGitWalk *is*: at 4 000 the deep row
	// finds a branch, at 4 the shallow one loses it, and both are only caught
	// because 20 and 41 sit either side.
	if maxGitWalk <= 20 || maxGitWalk >= 41 {
		t.Fatalf("maxGitWalk = %d, want between 21 and 40: the depth fixtures below are a literal 20 (must be found) and a literal 41 (must not be)", maxGitWalk)
	}

	for _, tc := range []struct{ name, dir, want string }{
		{"normal checkout", normal, "main"},
		{"branch with a slash", slashed, "feat/x"},
		{"detached, sha-1 repository", detached1, "@" + head1[:7]},
		{"detached, sha-256 repository", detached256, "@" + head256[:7]},
		{"unborn", unborn, "master"},
		{"worktree, absolute gitdir pointer", worktree, "wtbranch"},
		{"submodule, relative gitdir pointer", submodule, "subbranch"},
		{"submodule, from a directory inside it", submoduleNested, "subbranch"},
		{"bare repository", bare, ""},
		{"not a repository", plain, ""},
		{"child directory of a repository", child, "main"},
		{"twenty levels down, still found", deepFound, "main"},
		{"forty-one levels down, the walk gives up", deepGivenUp, ""},
		{"git directory with no HEAD", noHead, ""},
		{"gitdir pointer to nowhere", brokenPointer, ""},
		{"a .git file that is not a pointer", garbagePointer, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitBranch(osFS{}, tc.dir).branch; got != tc.want {
				t.Errorf("gitBranch(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		})
	}
}

// gitBranch must never walk from the daemon's own working directory. A pane
// path arrives absolute; a relative or empty one is refused rather than
// resolved, because filepath.Join("", ".git") is ".git" and the daemon very
// often runs inside a checkout of something.
func TestGitBranchRefusesARelativePath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	repo := t.TempDir()
	requireNoRepoAbove(t, repo)
	git(t, repo, "init", "-q", "-b", "main", ".")
	commit(t, repo, "a")
	mkdir(t, repo, "child")
	t.Chdir(repo)

	// The pre-state assertion. Without it the rows below would pass in a
	// directory that is not a repository at all, which is the wrong reason.
	if got := gitBranch(osFS{}, repo).branch; got != "main" {
		t.Fatalf("gitBranch(%q) = %q, want %q: the working directory for the rows below is not a discoverable repository, so they would say nothing", repo, got, "main")
	}
	for _, in := range []string{"", ".", "child", "./child"} {
		t.Run(in, func(t *testing.T) {
			if got := gitBranch(osFS{}, in).branch; got != "" {
				t.Errorf("gitBranch(%q) = %q, want %q", in, got, "")
			}
		})
	}
}

// The parser on its own, over literal file contents. The hashes here are typed
// rather than taken from a fixture: the wants are exact strings, so nothing on
// the assertion side can move with the code under test.
func TestGitParseHead(t *testing.T) {
	const sha1Head = "f2aa39a1cd0f4b6de2a4e3c7b8195d0e6a7c4f13"
	const sha256Head = "f2aa39a1cd0f4b6de2a4e3c7b8195d0e6a7c4f13f2aa39a1cd0f4b6de2a4e3c7"

	for _, tc := range []struct{ name, in, want string }{
		// Every HEAD git writes ends in a newline, so the trim is exercised by
		// the ordinary rows and not only by a row invented for it.
		{"normal checkout, trailing newline", "ref: refs/heads/main\n", "main"},
		{"no trailing newline", "ref: refs/heads/main", "main"},
		{"crlf", "ref: refs/heads/main\r\n", "main"},
		// The prefix is stripped whole. Taking the last path element instead
		// would answer "x" here.
		{"branch with a slash", "ref: refs/heads/feat/x\n", "feat/x"},
		{"branch with two slashes", "ref: refs/heads/user/feat/x\n", "user/feat/x"},
		// Seven characters after an "@", so a detached HEAD cannot be read as
		// somebody's branch named f2aa39a.
		{"detached, 40 hex", sha1Head + "\n", "@f2aa39a"},
		{"detached, 64 hex", sha256Head + "\n", "@f2aa39a"},
		// Only refs/heads. A HEAD holding anything else is not a branch this
		// row can name, and stripping "refs/" alone would answer
		// "remotes/origin/main" here.
		{"a ref that is not a branch", "ref: refs/remotes/origin/main\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHead(tc.in); got != tc.want {
				t.Errorf("parseHead(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGitParseHeadRefusesATornRead(t *testing.T) {
	// `git checkout` rewrites .git/HEAD, and a read that lands mid-write gets a
	// short or torn line. The parser accepts only "ref: refs/..." or exactly 40
	// or 64 hex characters and shows nothing otherwise; the next pass, one poll
	// later, reads the settled file.
	const hex40 = "f2aa39a1cd0f4b6de2a4e3c7b8195d0e6a7c4f13"
	const hex64 = "f2aa39a1cd0f4b6de2a4e3c7b8195d0e6a7c4f13f2aa39a1cd0f4b6de2a4e3c7"

	for _, tc := range []struct{ name, in string }{
		{"truncated ref line", "ref: refs/hea"},
		{"empty", ""},
		{"a six-character stub", "f2aa39"},
		// One short and one long, either side of both accepted lengths. These
		// are what stop "any run of hex is a hash" from passing.
		{"thirty-nine hex", hex40[:39]},
		{"forty-one hex", hex40 + "f"},
		{"sixty-three hex", hex64[:63]},
		{"sixty-five hex", hex64 + "f"},
		// A ref line with no name after it.
		{"ref with an empty name", "ref: refs/heads/"},
		// Forty characters, not all of them hex.
		{"forty characters with a non-hex digit", "g2aa39a1cd0f4b6de2a4e3c7b8195d0e6a7c4f13"},
		// Git writes hashes in lower case. Upper case is not something a real
		// HEAD contains, so it is refused rather than guessed at.
		{"forty upper-case hex", strings.ToUpper(hex40)},
		// A whole second line means this is not a HEAD file.
		{"two lines", "ref: refs/heads/main\nref: refs/heads/other\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHead(tc.in); got != "" {
				t.Errorf("parseHead(%q) = %q, want %q", tc.in, got, "")
			}
		})
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// The owner's own git configuration is not part of this test's subject and
	// could change the default branch name, install hooks, or template a
	// repository. Both config layers go to /dev/null and the identity comes
	// from the environment, so nothing here reads or writes anything outside
	// t.TempDir().
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=tmux-web test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=tmux-web test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, name string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, name), name+"\n")
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", name)
}

// mkdirDepth builds n nested directories below parent and returns the deepest.
// mkdir itself lives in snapshot_path_internal_test.go.
func mkdirDepth(t *testing.T, parent, prefix string, n int) string {
	t.Helper()
	dir := parent
	for i := 1; i <= n; i++ {
		dir = mkdir(t, dir, prefix+strconv.Itoa(i))
	}
	return dir
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireGitIsAFile(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatalf("stat %s/.git: %v", dir, err)
	}
	if info.IsDir() {
		t.Fatalf("%s/.git is a directory: this git does not use a gitdir pointer here, and the row below would test the wrong thing", dir)
	}
}

func requireNoRepoAbove(t *testing.T, dir string) {
	t.Helper()
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			t.Fatalf("%s sits inside a working tree (%s/.git): the not-a-repository rows cannot mean anything here", dir, d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return
		}
		d = parent
	}
}
