package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/integrations"
)

// Task 20's tests. Every one of them installs into a directory it created
// itself, and that is a safety property rather than hygiene: this command
// writes executable code into a project, and the three agent configurations on
// a developer's machine -- ~/.claude/settings.json,
// ~/.config/opencode/opencode.jsonc, ~/.pi/agent/settings.json -- are files a
// test has no business discovering. Nothing here reads $HOME without setting it
// first.

// installCLI drives the CLI in-process with a stdin of its own, because the
// confirmation prompt is the one thing in this command that reads one.
func installCLI(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errb strings.Builder
	code := runInstall(args, strings.NewReader(stdin), &out, &errb)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// TestInstallRefusesAFileItDoesNotOwn is the managed header's whole reason for
// existing. A file in .pi/extensions/ that we did not write belongs to
// somebody, and overwriting it is the one failure this installer must never
// have.
func TestInstallRefusesAFileItDoesNotOwn(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".pi", "extensions", "wterm.ts")
	const theirs = "// my own extension, and it is not yours\nexport default function () {}\n"
	installWrite(t, path, theirs)

	r := installCLI(t, "", "--agent", "pi", "--yes", root)

	r.wantCode(t, 1)
	if !strings.Contains(r.stderr, path) {
		t.Errorf("the refusal does not name the file it refused:\n%s", r)
	}
	if got := installRead(t, path); got != theirs {
		t.Errorf("the file was modified:\n%q", got)
	}
}

// TestInstallOverwritesItsOwnFile is the other two cases the schema line buys,
// and they are different cases: ours-and-current is a silent overwrite, and
// ours-but-older says so.
//
// The mutant this is aimed at is matching the header by prefix -- "managed by
// tmux-web" -- rather than by the schema number. That mutant keeps
// TestInstallRefusesAFileItDoesNotOwn green (a foreign file still has no
// header at all) and quietly loses the ability to tell a user their
// integration just changed shape under them.
//
// THE ASSERTION IS THE WHOLE MESSAGE, not the word "older", and the first
// version of this test got that wrong in a way that let the prefix mutant
// through. t.TempDir() names the directory after the test -- so a subtest
// called "ours but older" installs into a path with "older" in it, which every
// run prints, which made `strings.Contains(stderr, "older")` true
// unconditionally. The subtests below are named so that no fixture path can
// contain the phrase either.
func TestInstallOverwritesItsOwnFile(t *testing.T) {
	// What the ours-but-stale case must say, and it cannot appear in a path.
	const staleNote = "written by an older tmux-web (wterm-schema: 0)"

	t.Run("ours and current, silently", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, ".opencode", "plugin", "wterm.js")

		first := installCLI(t, "", "--agent", "opencode", "--yes", root)
		first.wantCode(t, 0)
		want := installRead(t, path)

		second := installCLI(t, "", "--agent", "opencode", "--yes", root)
		second.wantCode(t, 0)
		if got := installRead(t, path); got != want {
			t.Errorf("reinstalling changed the file")
		}
		if strings.Contains(second.stderr, "wterm-schema:") {
			t.Errorf("reinstalling the current schema reported a schema change:\n%s", second)
		}
	})

	t.Run("ours but stale, and it says so", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, ".opencode", "plugin", "wterm.js")
		installWrite(t, path, "// managed by tmux-web (wterm-schema: 0)\nexport default function () {}\n")

		r := installCLI(t, "", "--agent", "opencode", "--yes", root)

		r.wantCode(t, 0)
		if !strings.Contains(r.stderr, staleNote) {
			t.Errorf("replacing a file written by an earlier schema did not say %q:\n%s", staleNote, r)
		}
		if got := installRead(t, path); !strings.Contains(got, "wterm-schema: 1") {
			t.Errorf("the older file was not replaced:\n%.200q", got)
		}
	})
}

// TestClaudeMergePreservesTheUsersOwnHooks. settings.json is a file the user
// owns and this command is a guest in it.
//
// The comparison is SEMANTIC -- parsed JSON, not bytes -- and that is a scoping
// decision rather than a slack assertion. encoding/json preserves neither key
// order nor formatting, so promising the file back byte-for-byte is promising a
// format-preserving JSON editor, which is not scoped here. What bounds the
// damage instead is the rule set: valid JSON or refuse, our entries only, named
// by our command, and the no-op case below that must not write at all.
func TestClaudeMergePreservesTheUsersOwnHooks(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, ".claude", "settings.json")
	installWrite(t, settings, `{
  "model": "opusmagnum",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/my-audit.sh"}]}
    ]
  }
}
`)

	installCLI(t, "", "--agent", "claude", "--yes", root).wantCode(t, 0)

	after := parseSettings(t, settings)
	if after["model"] != "opusmagnum" {
		t.Errorf(`the user's unrelated top-level key did not survive the merge: model = %v`, after["model"])
	}
	hooks := hooksOf(t, after)
	for _, event := range integrations.ClaudeHookEvents {
		if len(hooks[event]) == 0 {
			t.Errorf("after install, hooks.%s is empty", event)
		}
	}
	pre := hooks["PreToolUse"]
	if len(pre) != 2 {
		t.Fatalf("hooks.PreToolUse has %d entries, want the user's plus ours: %v", len(pre), pre)
	}
	if !hasCommand(pre, "/usr/local/bin/my-audit.sh") {
		t.Errorf("the user's own PreToolUse hook is gone: %v", pre)
	}

	// And back out again. `--remove` matching by hook NAME rather than by our
	// command would take the user's entry with it, which is what the survivor
	// below is checked for.
	installCLI(t, "", "--agent", "claude", "--yes", "--remove", root).wantCode(t, 0)

	after = parseSettings(t, settings)
	if after["model"] != "opusmagnum" {
		t.Errorf(`--remove lost the user's unrelated top-level key: model = %v`, after["model"])
	}
	hooks = hooksOf(t, after)
	if pre := hooks["PreToolUse"]; len(pre) != 1 || !hasCommand(pre, "/usr/local/bin/my-audit.sh") {
		t.Errorf("--remove did not leave the user's own PreToolUse hook exactly as it was: %v", pre)
	}
	for _, event := range integrations.ClaudeHookEvents {
		for _, entry := range hooks[event] {
			if strings.Contains(commandsOf(entry), claudeScriptName) {
				t.Errorf("--remove left one of ours behind in hooks.%s: %v", event, entry)
			}
		}
	}
}

// TestAMergeThatChangesNothingDoesNotTouchTheFile is the one place a byte
// assertion belongs.
//
// It is what stops "it only reformats the file when something changed" from
// becoming "it reformats the file every time anybody runs it" -- the
// wholesale-rewrite mutant wearing a different hat. The mtime is set into the
// past first so that the assertion is meaningful without a sleep.
func TestAMergeThatChangesNothingDoesNotTouchTheFile(t *testing.T) {
	t.Run("a second install", func(t *testing.T) {
		root := t.TempDir()
		settings := filepath.Join(root, ".claude", "settings.json")
		installCLI(t, "", "--agent", "claude", "--yes", root).wantCode(t, 0)

		before, mtime := freezeFile(t, settings)
		installCLI(t, "", "--agent", "claude", "--yes", root).wantCode(t, 0)
		assertUntouched(t, settings, before, mtime)
	})

	t.Run("a remove with none of ours present", func(t *testing.T) {
		root := t.TempDir()
		settings := filepath.Join(root, ".claude", "settings.json")
		// Deliberately ugly: four-space indent, an unusual key order and a
		// trailing blank line. A run that rewrites it would "fix" all three.
		installWrite(t, settings, "{\n    \"z\": 1,\n    \"a\": {\"deep\": [1,2,3]}\n}\n\n")

		before, mtime := freezeFile(t, settings)
		installCLI(t, "", "--agent", "claude", "--yes", "--remove", root).wantCode(t, 0)
		assertUntouched(t, settings, before, mtime)
	})
}

// TestClaudeRefusesInvalidJSON. Not valid JSON: refuse, do not rewrite, do not
// "fix". The file is the user's.
//
// It also asserts the refusal happens BEFORE anything is written: the wrapper
// script is a file of ours in the same directory, and a run that wrote it and
// then failed on the settings would leave a hook script behind that nothing
// calls.
func TestClaudeRefusesInvalidJSON(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, ".claude", "settings.json")
	const broken = "{ \"hooks\": { oh dear\n"
	installWrite(t, settings, broken)

	r := installCLI(t, "", "--agent", "claude", "--yes", root)

	r.wantCode(t, 1)
	if !strings.Contains(r.stderr, settings) {
		t.Errorf("the refusal does not name the file:\n%s", r)
	}
	if got := installRead(t, settings); got != broken {
		t.Errorf("the user's file was rewritten:\n%q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", claudeScriptName)); err == nil {
		t.Errorf("the wrapper script was written even though the install was refused")
	}
}

// TestInstallIsNeverReachableFromHTTP.
//
// NOT a route-table assertion. http.ServeMux exposes no way to enumerate its
// patterns, so "no handler anywhere serves installation" is not a question any
// Go test can ask it. Two things stand in its place, and being honest about
// which is which matters more than the test:
//
//  1. The compiler already enforces the direct half. This installer lives in
//     `package main`, and a package main cannot be imported -- so internal/front
//     CANNOT call it, today or ever, without somebody first moving it. That is a
//     stronger guarantee than any assertion here.
//  2. This test covers the other half: a REIMPLEMENTATION inside internal/front.
//     It is a grep. It reads internal/front's .go files and fails if the install
//     symbols appear there at all. A grep is a blunt instrument and this comment
//     says so plainly rather than dressing it up: what it really buys is that
//     somebody adding an install route has to delete a test with this comment in
//     it.
//
// The prohibition itself also goes where the routes are built, in a comment at
// internal/front/server.go's mux, because that is where the person who would
// add the route is reading.
func TestInstallIsNeverReachableFromHTTP(t *testing.T) {
	needles := []string{
		"install-integration",
		"claudehookblock",
		"wterm-schema",
		".pi/extensions",
		".opencode/plugin",
		".claude/settings.json",
	}

	dir := filepath.Join("..", "..", "internal", "front")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	// A grep that greps nothing is the easiest vacuous test in this plan to
	// write by accident, so the file count is asserted too.
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		scanned++
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		lower := strings.ToLower(string(body))
		for _, needle := range needles {
			if strings.Contains(lower, needle) {
				t.Errorf("%s mentions %q. Installation writes executable code into a repository, and that is not a thing a network request should be able to do however well authenticated it is: it is a CLI act, in package main, which internal/front cannot import. If this is a comment saying exactly that, the needle list in this test is what needs the exception -- but read the comment at internal/front/server.go's mux first.",
					path, needle)
			}
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned %d .go files under %s; this grep is looking at nothing", scanned, dir)
	}
}

// TestNoGlobalForOpencode. Open question 5: opencode MAY have a global plugin
// directory at ~/.config/opencode/plugin/. Nobody has verified it, the design
// says in as many words that it must not be implemented on the strength of its
// own sentence, and a --global that silently installed per project instead
// would be worse than one that refuses.
func TestNoGlobalForOpencode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No directory operand, deliberately: with one, --global could be refused
	// for taking an argument it does not take, and this test would pass over a
	// build that installs globally for opencode quite happily.
	r := installCLI(t, "", "--agent", "opencode", "--global", "--yes")

	r.wantCode(t, 2)
	if !strings.Contains(r.stderr, "unverified") {
		t.Errorf("--global --agent opencode was refused without saying why:\n%s", r)
	}
	assertNothingUnder(t, home)
}

// TestNoGlobalForPi, for the same reason and a different mechanism: pi's
// project-local extensions are discovered in .pi/extensions/, and a global pi
// extension is registered in the user's own settings.json by `pi install`. That
// is a file we would then be editing on pi's behalf, and nobody has verified
// its shape either.
func TestNoGlobalForPi(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	r := installCLI(t, "", "--agent", "pi", "--global", "--yes")

	r.wantCode(t, 2)
	if !strings.Contains(r.stderr, "unverified") {
		t.Errorf("--global --agent pi was refused without saying why:\n%s", r)
	}
	assertNothingUnder(t, home)
}

// TestGlobalIsOfferedForClaude, which is the one agent whose hooks are
// documented to live in a user-level settings.json.
func TestGlobalIsOfferedForClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	installCLI(t, "", "--agent", "claude", "--global", "--yes").wantCode(t, 0)

	settings := filepath.Join(home, ".claude", "settings.json")
	hooks := hooksOf(t, parseSettings(t, settings))
	if len(hooks["Stop"]) != 1 {
		t.Fatalf("a global claude install wrote no Stop hook into %s", settings)
	}
}

// TestWithoutYesItAsksAndWritesNothing covers two mutants at once: skipping the
// confirmation, and writing before printing the paths. Both leave a file behind
// after a run the user declined.
func TestWithoutYesItAsksAndWritesNothing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".pi", "extensions", "wterm.ts")

	r := installCLI(t, "n\n", "--agent", "pi", root)

	r.wantCode(t, 1)
	if !strings.Contains(r.stdout, path) {
		t.Errorf("the exact path was not printed before asking:\n%s", r)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("declining the confirmation still wrote %s", path)
	}

	// An empty stdin is a NO. This is the unattended case -- a script, a cron
	// entry, `< /dev/null` -- and a confirmation that reads EOF as consent is
	// not a confirmation.
	installCLI(t, "", "--agent", "pi", root).wantCode(t, 1)
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("an empty stdin was taken for a yes and wrote %s", path)
	}

	// And the same command with a yes on stdin does write it, so the refusals
	// above are the confirmation working rather than the install being broken.
	installCLI(t, "y\n", "--agent", "pi", root).wantCode(t, 0)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("confirming did not write %s: %v", path, err)
	}
}

// TestTheInstalledClaudeScriptIsExecutable, and the failure it guards against
// is SILENT.
//
// embed.FS carries no file mode. The repo's copy is committed 0755 as a signal
// and that signal does not survive embedding, so a script written 0644 fails
// with exit 126 -- and because every hook this project registers is
// `"async": true`, Claude ignores the exit code entirely. Nothing anywhere
// reports a problem: the pane simply stops being reported on.
func TestTheInstalledClaudeScriptIsExecutable(t *testing.T) {
	root := t.TempDir()
	installCLI(t, "", "--agent", "claude", "--yes", root).wantCode(t, 0)

	script := filepath.Join(root, ".claude", claudeScriptName)
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("the wrapper was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o111 == 0 {
		t.Errorf("the installed wrapper is mode %04o: an async hook's exit 126 is IGNORED by Claude, so a non-executable wrapper is total silence", perm)
	}

	body := installRead(t, script)
	if strings.Contains(body, integrations.ClaudeBinPlaceholder) {
		t.Errorf("the installed wrapper still carries %s: the `[ -x \"$BIN\" ]` guard then exits 0 on every hook and nothing is ever reported",
			integrations.ClaudeBinPlaceholder)
	}
	bin := binOf(t, body)
	if !filepath.IsAbs(bin) {
		t.Errorf("BIN=%q is not an absolute path; a hook runs with the agent's cwd, not ours", bin)
	}
	if info, err := os.Stat(bin); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("BIN=%q is not an executable file (%v): the guard in the wrapper would exit 0 for the life of the install", bin, err)
	}
}

// TestTheHookCommandNamesTheScriptTheInstallerWrote is the cross-check Task 19
// could not make, because internal/integrations cannot see where its own bytes
// end up. A settings.json pointing at a path we did not write is not an error
// anywhere: the wrapper's own guard makes a missing BIN silent, and a missing
// WRAPPER makes the shell exit 127 into a hook whose exit code Claude ignores.
// Two silences in a row is exactly the failure this project keeps finding.
func TestTheHookCommandNamesTheScriptTheInstallerWrote(t *testing.T) {
	root := t.TempDir()
	installCLI(t, "", "--agent", "claude", "--yes", root).wantCode(t, 0)

	script := filepath.Join(root, ".claude", claudeScriptName)
	hooks := hooksOf(t, parseSettings(t, filepath.Join(root, ".claude", "settings.json")))
	if len(hooks) != len(integrations.ClaudeHookEvents) {
		t.Errorf("settings.json holds %d hook events, want the %d in integrations.ClaudeHookEvents",
			len(hooks), len(integrations.ClaudeHookEvents))
	}
	for _, event := range integrations.ClaudeHookEvents {
		entries := hooks[event]
		if len(entries) != 1 {
			t.Fatalf("hooks.%s has %d entries, want one", event, len(entries))
		}
		cmd := commandsOf(entries[0])
		// Shell-quoted, because Claude runs the command through a shell and an
		// install prefix with a space in it would otherwise split into two
		// words -- a wrapper nobody can find, silently.
		if want := "'" + script + "' " + event; cmd != want {
			t.Errorf("hooks.%s command = %q, want %q", event, cmd, want)
		}
	}
}

// TestTheInstalledPluginIsOneSelfContainedFile is the distribution decision,
// and it is the one thing this task had to settle by measurement.
//
// MEASURED, opencode 1.18.30 and pi 0.85.1, in throwaway projects under
// throwaway HOME/XDG directories:
//
//   - opencode loads EVERY file in .opencode/plugin/ and calls EVERY EXPORTED
//     FUNCTION of each as a plugin factory -- not just the default. A
//     queue module written beside the plugin is therefore a second plugin, and
//     the plugin's own `handlers` export is a third: registered with opencode's
//     plugin input in place of the `report` function, it throws inside the hook
//     chain. Measured with the real file: verbatim, one malformed report and
//     the rest of the turn's reports lost; with the named exports stripped, the
//     turn reported normally.
//   - pi REFUSES TO START when a file in .pi/extensions/ exports no factory:
//     `Failed to load extension ".../wterm-queue.ts": Extension does not export
//     a valid factory function`, and a hint to restart with -ne.
//
// So the queue is inlined and every export but the default is stripped. One
// source of truth stays in internal/integrations/queue.ts; the concatenation
// happens in Go.
func TestTheInstalledPluginIsOneSelfContainedFile(t *testing.T) {
	for _, tc := range []struct{ agent, path string }{
		{"pi", filepath.Join(".pi", "extensions", "wterm.ts")},
		{"opencode", filepath.Join(".opencode", "plugin", "wterm.js")},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			root := t.TempDir()
			installCLI(t, "", "--agent", tc.agent, "--yes", root).wantCode(t, 0)

			dir := filepath.Dir(filepath.Join(root, tc.path))
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("reading %s: %v", dir, err)
			}
			if len(entries) != 1 || entries[0].Name() != filepath.Base(tc.path) {
				t.Fatalf("%s holds %d entries, want exactly our own file: pi refuses to start when a file there exports no factory, and opencode calls every export of every file in its plugin directory as a plugin",
					dir, len(entries))
			}

			body := installRead(t, filepath.Join(root, tc.path))
			if strings.Contains(body, "./queue.ts") {
				t.Errorf("the installed file still imports ./queue.ts, which is not there")
			}
			for _, want := range []string{"function makeQueue(", "function spawnReport("} {
				if !strings.Contains(body, want) {
					t.Errorf("the installed file does not carry %q: both integrations call it on every event", want)
				}
			}
			exports := regexp.MustCompile(`(?m)^export .*$`).FindAllString(body, -1)
			if len(exports) != 1 || !strings.HasPrefix(exports[0], "export default") {
				t.Errorf("the installed file's top-level exports are %q; want exactly one, `export default`. opencode calls every named export as a plugin factory: `handlers` returns a hooks object built over opencode's plugin input instead of a report function, and the turn's reports are lost.",
					exports)
			}
		})
	}
}

// TestInstallWarnsAboutOpencodesFootprint. "Why is there a .gitignore in my
// client's repo" is a question the user should be able to answer without
// archaeology, and the answer has to arrive BEFORE they say yes.
//
// MEASURED on opencode 1.18.30: a first run with a plugin present created
// .opencode/package.json, .opencode/package-lock.json, .opencode/node_modules/
// and .opencode/.gitignore (listing node_modules, package.json,
// package-lock.json, bun.lock and .gitignore itself).
func TestInstallWarnsAboutOpencodesFootprint(t *testing.T) {
	root := t.TempDir()
	r := installCLI(t, "n\n", "--agent", "opencode", root)

	r.wantCode(t, 1)
	for _, want := range []string{".gitignore", "node_modules"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the warning shown before the confirmation does not mention %q:\n%s", want, r)
		}
	}
}

// TestInstallWarnsAboutANestedInstall. Open question 8: two integrations
// writing one pane option is a race nobody has looked at. The instruction is to
// warn where it can be detected and not to attempt a resolution.
func TestInstallWarnsAboutANestedInstall(t *testing.T) {
	parent := t.TempDir()
	installCLI(t, "", "--agent", "opencode", "--yes", parent).wantCode(t, 0)

	child := filepath.Join(parent, "packages", "inner")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	r := installCLI(t, "", "--agent", "pi", "--yes", child)

	r.wantCode(t, 0)
	if !strings.Contains(r.stderr, filepath.Join(parent, ".opencode", "plugin", "wterm.js")) {
		t.Errorf("installing below an installed project did not name the ancestor's integration:\n%s", r)
	}
}

// TestRemoveLeavesNothingOfOurs, for the two file-based agents. `rm` is
// documented as working too, and an integration you cannot remove with `rm` is
// one you have to trust more than this one deserves.
func TestRemoveLeavesNothingOfOurs(t *testing.T) {
	for _, tc := range []struct{ agent, path string }{
		{"pi", filepath.Join(".pi", "extensions", "wterm.ts")},
		{"opencode", filepath.Join(".opencode", "plugin", "wterm.js")},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			root := t.TempDir()
			installCLI(t, "", "--agent", tc.agent, "--yes", root).wantCode(t, 0)
			installCLI(t, "", "--agent", tc.agent, "--yes", "--remove", root).wantCode(t, 0)

			if _, err := os.Stat(filepath.Join(root, tc.path)); err == nil {
				t.Errorf("--remove left %s behind", tc.path)
			}
		})
	}

	t.Run("claude", func(t *testing.T) {
		root := t.TempDir()
		installCLI(t, "", "--agent", "claude", "--yes", root).wantCode(t, 0)
		installCLI(t, "", "--agent", "claude", "--yes", "--remove", root).wantCode(t, 0)

		if _, err := os.Stat(filepath.Join(root, ".claude", claudeScriptName)); err == nil {
			t.Errorf("--remove left the wrapper script behind")
		}
		if hooks := hooksOf(t, parseSettings(t, filepath.Join(root, ".claude", "settings.json"))); len(hooks) != 0 {
			t.Errorf("--remove left hooks behind: %v", hooks)
		}
	})
}

// TestRemoveRefusesAFileItDoesNotOwn, because `--remove` deletes and the
// ownership rule is not weaker in that direction.
func TestRemoveRefusesAFileItDoesNotOwn(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".opencode", "plugin", "wterm.js")
	const theirs = "export default function () {}\n"
	installWrite(t, path, theirs)

	r := installCLI(t, "", "--agent", "opencode", "--yes", "--remove", root)

	r.wantCode(t, 1)
	if got := installRead(t, path); got != theirs {
		t.Errorf("--remove deleted or changed a file it does not own")
	}
}

// TestUnknownAgentIsRefused, because the one thing worse than no integration is
// an integration installed for an agent that does not read it.
func TestUnknownAgentIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"--yes"},
		{"--agent", "cursor", "--yes"},
	} {
		r := installCLI(t, "", append(args, t.TempDir())...)
		r.wantCode(t, 2)
	}
}

// -- fixture ----------------------------------------------------------------

func installWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// freezeFile reads a file and stamps it into the past, so that "this run did
// not touch it" is an assertion rather than a race against the clock's
// resolution.
func freezeFile(t *testing.T, path string) (string, time.Time) {
	t.Helper()
	body := installRead(t, path)
	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	return body, past
}

func assertUntouched(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if got := installRead(t, path); got != body {
		t.Errorf("a run that changes nothing rewrote %s:\n have %q\n want %q", path, got, body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(mtime) {
		t.Errorf("a run that changes nothing rewrote %s: mtime moved from %s to %s", path, mtime, info.ModTime())
	}
}

// assertNothingUnder fails if a directory holds anything at all. It is how the
// refusals prove they refused before writing.
func assertNothingUnder(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s is not empty: %v", dir, entries)
	}
}

func parseSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(installRead(t, path)), &m); err != nil {
		t.Fatalf("%s is not valid JSON after the merge: %v", path, err)
	}
	return m
}

// hooksOf digs the hooks object out of a parsed settings.json, as a map of
// event name to its matcher entries.
func hooksOf(t *testing.T, settings map[string]any) map[string][]any {
	t.Helper()
	out := map[string][]any{}
	raw, ok := settings["hooks"]
	if !ok {
		return out
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("hooks is not an object: %T", raw)
	}
	for event, v := range hooks {
		entries, ok := v.([]any)
		if !ok {
			t.Fatalf("hooks.%s is not a list: %T", event, v)
		}
		out[event] = entries
	}
	return out
}

// commandsOf joins every command string in one matcher entry, so an assertion
// can be written against the thing that identifies an entry as ours.
func commandsOf(entry any) string {
	m, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	hooks, ok := m["hooks"].([]any)
	if !ok {
		return ""
	}
	var out []string
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok {
			out = append(out, cmd)
		}
	}
	return strings.Join(out, " ")
}

func hasCommand(entries []any, want string) bool {
	for _, e := range entries {
		if strings.Contains(commandsOf(e), want) {
			return true
		}
	}
	return false
}

// binOf pulls the resolved binary path back out of the installed wrapper.
func binOf(t *testing.T, script string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^BIN='(.*)'$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("the installed wrapper has no BIN assignment:\n%s", script)
	}
	return strings.ReplaceAll(m[1], `'\''`, `'`)
}
