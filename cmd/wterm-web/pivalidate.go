package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// The pre-flight for `--global --agent pi`, and the reason it is the only
// install in this command that has one.
//
// A pi extension in $PI_CODING_AGENT_DIR/extensions/ auto-loads in EVERY
// project on the machine -- no settings entry, no trust prompt, which makes it
// LESS gated than a project-local extension, not more: that one needs
// `--approve`. Measured on pi 0.85.1, a file there that pi cannot load makes pi
// fail to start, exit 1, in every one of those projects:
//
//	Error: Failed to load extension "…/wterm.ts": Extension does not export a
//	valid factory function: …
//	Hint: Start without extensions using "pi -ne".
//
// Four shapes provoke it and all four are reachable from this repository by
// accident: no default export, a default export that is not a function, a parse
// error, and a factory that throws. install.go's stripExports rewrites every
// export line in the file on its way out, so "we generated it, so it is fine"
// is exactly the assumption that would put a broken file there. The static
// check in inlineQueue is a regexp over text; this is pi's own loader.
//
// WHAT THE PROBE IS. A throwaway PI_CODING_AGENT_DIR holding one file -- the
// artifact this install is about to write -- and a real `pi` started against
// it, non-interactively, with a PROVIDER NAME THAT CANNOT EXIST. That last part
// is the whole trick:
//
//   - `Unknown provider "…"` is printed AFTER every extension has been loaded
//     and BEFORE anything reaches a network, so it is a positive control: seeing
//     it is proof that pi got past extension loading with no complaint about our
//     file. A bare exit code cannot say that -- pi exits 1 either way.
//   - and it means the probe never calls a model, never needs a credential, and
//     never makes a network request. Measured: 0-1 s, offline.
//
// Modes that exit sooner were tried and rejected. `pi --help` and
// `pi --list-models` DO load and call extensions -- an extension can register a
// CLI flag -- but they tolerate one that fails: exit 0, nothing on stderr. A
// probe built on either would pass a file that stops pi starting.

// piProbeProvider is the provider name the probe pins pi to. It cannot be a
// real one, and it appears in pi's refusal verbatim, so it is also the sentinel
// piProbeVerdict looks for.
const piProbeProvider = "wterm-web-install-probe"

// piProbeTimeout. Measured at 0-1 s; this is the budget for a cold node on a
// loaded machine, after which the verdict is piUnknown and the install is
// refused. A probe that hangs must not become an install that proceeds.
const piProbeTimeout = 90 * time.Second

// piVerdict is what one probe run concluded.
type piVerdict int

const (
	// piUnknown is the ZERO VALUE on purpose: every way of failing to reach an
	// answer -- pi missing, exec refused, a timeout, output nobody recognises
	// -- lands here, and here is the refusing side.
	piUnknown piVerdict = iota
	piLoads
	piRefuses
)

func (v piVerdict) String() string {
	switch v {
	case piLoads:
		return "pi loaded it"
	case piRefuses:
		return "pi refused it"
	}
	return "no answer"
}

// piProbe is what validatePiArtifact runs. It is a variable so that the tests
// which are not about the probe can replace it: they assert on paths and
// messages, and every one of them would otherwise start a real pi.
var piProbe = runPiProbe

// runPiProbe loads the artifact once, with the real pi, and says what happened.
//
// It never touches the user's own pi: HOME and PI_CODING_AGENT_DIR are built
// fresh under the system temporary directory and removed afterwards, and the
// environment is constructed rather than inherited so that no credential of
// theirs can be picked up by a process this command started.
func runPiProbe(artifact []byte) (piVerdict, string) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		return piUnknown, "pi is not on PATH (" + err.Error() + ")"
	}
	dir, err := os.MkdirTemp("", "wterm-pi-probe-")
	if err != nil {
		return piUnknown, "cannot make a directory to try it in: " + err.Error()
	}
	defer os.RemoveAll(dir)

	agent := filepath.Join(dir, "agent")
	ext := filepath.Join(agent, "extensions", "wterm.ts")
	home := filepath.Join(dir, "home")
	work := filepath.Join(dir, "work")
	for _, d := range []string{filepath.Dir(ext), home, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return piUnknown, "cannot make a directory to try it in: " + err.Error()
		}
	}
	if err := os.WriteFile(ext, artifact, 0o644); err != nil {
		return piUnknown, "cannot write the file to try: " + err.Error()
	}

	ctx, cancel := context.WithTimeout(context.Background(), piProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--provider", piProbeProvider, "--model", piProbeProvider,
		"--no-session", "-p", "wterm-web install probe")
	cmd.Dir = work
	cmd.Env = piProbeEnv(home, agent)
	// A nil Stdin is /dev/null, and it has to be: with a terminal on stdin this
	// invocation waits instead of exiting, which is a timeout and a refusal.
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return piUnknown, fmt.Sprintf("pi did not finish within %s", piProbeTimeout)
	}
	if len(out) == 0 && err != nil {
		return piUnknown, "pi could not be run: " + err.Error()
	}
	return piProbeVerdict(out, ext)
}

// piProbeEnv is the whole environment the probe's pi gets. It is BUILT AND NOT
// INHERITED, and that is the safety property this file turns on rather than
// tidiness -- which is why it is a function of its own with a test against it,
// instead of five lines inside the exec call where the only thing that could
// check them is a side effect pi may or may not produce.
//
//   - HOME and PI_CODING_AGENT_DIR are the probe's own throwaway directories,
//     so a pi started by an INSTALLER cannot read or write the user's real
//     ~/.pi/agent: their extensions, their auth.json, their sessions.
//   - Nothing else is carried but PATH, which pi needs because it is a node
//     program. In particular no provider credential of theirs reaches a process
//     this command started -- and it has no use for one: the probe pins a
//     provider that does not exist and never reaches a model.
func piProbeEnv(home, agent string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"PI_CODING_AGENT_DIR=" + agent,
		"PI_OFFLINE=1",
		"TERM=dumb",
	}
}

// piProbeVerdict reads one probe run's output. It is separated from the run so
// that pi's four refusals can be held against captured bytes on a machine with
// no pi installed.
//
// ORDER MATTERS, and the refusal is checked first: if pi ever printed both, the
// one that stops the install is the one that decides.
func piProbeVerdict(out []byte, ext string) (piVerdict, string) {
	// Both, not either. The message alone would call somebody else's broken
	// extension ours; the path alone appears in output that is not a failure.
	if bytes.Contains(out, []byte("Failed to load extension")) && bytes.Contains(out, []byte(ext)) {
		return piRefuses, firstLineContaining(out, "Failed to load extension")
	}
	if bytes.Contains(out, []byte(`Unknown provider "`+piProbeProvider+`"`)) {
		return piLoads, ""
	}
	return piUnknown, "pi printed nothing this command recognises: " + display(firstLineOf(out), 200)
}

// validatePiArtifact turns a verdict into the refusal the user reads, or nil.
//
// BOTH non-passing verdicts refuse, and the judgement in that is deliberate.
// The alternative -- install anyway and warn -- trades a message the user may
// not read for a pi that will not start in any project on the machine, and the
// repair needs somebody who knows both that `pi -ne` exists and which file to
// delete. The way through is named in the message instead: a project-scope
// install, which is not gated this way because a project-local extension that
// fails breaks that project only and pi prints the hint itself.
func validatePiArtifact(dir string, artifact []byte) error {
	verdict, detail := piProbe(artifact)
	switch verdict {
	case piLoads:
		return nil
	case piRefuses:
		return fmt.Errorf("pi refused to load the file this install would write, so it has not been written:\n"+
			"  %s\n"+
			"  an extension in %s that pi cannot load stops pi STARTING in every project on this machine,\n"+
			"  not just in one. This is a bug in tmux-web: please report it", detail, dir)
	}
	return fmt.Errorf("this install cannot be checked, so it has not been made: %s.\n"+
		"  a global pi extension auto-loads in every project on this machine, and one pi cannot load stops pi\n"+
		"  STARTING in all of them -- so this command loads the file with a real pi before writing it, and\n"+
		"  refuses when it cannot.\n"+
		"  put pi on PATH, or install into one project instead:\n"+
		"    wterm-web install-integration --agent pi <dir>\n"+
		"  a project-local extension that fails breaks that project only, and pi says how to start without it", detail)
}

// firstLineContaining is the one line of pi's output worth repeating.
func firstLineContaining(out []byte, needle string) string {
	for _, line := range bytes.Split(out, []byte("\n")) {
		if bytes.Contains(line, []byte(needle)) {
			return string(bytes.TrimSpace(line))
		}
	}
	return ""
}

func firstLineOf(out []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(out), []byte("\n"))
	return string(line)
}
