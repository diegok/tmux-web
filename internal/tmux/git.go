package tmux

import (
	"os"
	"path/filepath"
	"strings"
)

// maxGitWalk bounds the climb from a pane's directory to the filesystem root.
//
// Measured on the owner's live server: maximum pane path depth 7. Forty is
// slack, and its job is a pathological path or a symlink loop, not a real tree.
const maxGitWalk = 40

// gitBranch is what a row shows for a pane sitting in dir: a branch name, or
// "@" + seven hex characters when HEAD is detached, or "" for everything else
// -- a bare repository, no repository at all, or a HEAD it cannot make sense
// of. No fork: two reads at most, and usually one.
//
// dir must be absolute. tmux reports a pane's cwd out of /proc and it always
// is, and a relative one would be walked from the daemon's own working
// directory -- filepath.Join("", ".git") is ".git" -- which is very often a
// checkout of something else entirely.
func gitBranch(dir string) string {
	if !filepath.IsAbs(dir) {
		return ""
	}
	gitDir, ok := findGitDir(dir)
	if !ok {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
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
func findGitDir(dir string) (gitDir string, ok bool) {
	for i := 0; i < maxGitWalk; i++ {
		candidate := filepath.Join(dir, ".git")
		// Stat and not Lstat: a .git symlinked to a directory is still a git
		// directory, and a symlink loop comes back as an error and keeps the
		// climb going until maxGitWalk stops it.
		switch info, err := os.Stat(candidate); {
		case err == nil && info.IsDir():
			return candidate, true
		case err == nil:
			return resolveGitDirPointer(dir, candidate)
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
func resolveGitDirPointer(base, file string) (gitDir string, ok bool) {
	contents, err := os.ReadFile(file)
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
