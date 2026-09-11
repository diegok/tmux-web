package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/integrations"
)

// The pi pre-flight, and why this file exists at all.
//
// A pi extension in $PI_CODING_AGENT_DIR/extensions/ auto-loads in EVERY
// project on the machine, with no settings entry and no trust prompt. Measured
// on pi 0.85.1: a file there that pi cannot load makes pi FAIL TO START, exit
// 1, everywhere --
//
//	Error: Failed to load extension "…/wterm.ts": Extension does not export a
//	valid factory function: …
//	Hint: Start without extensions using "pi -ne".
//
// -- and the four shapes that provoke it are all reachable from this repository
// by accident: no default export, a default export that is not a function, a
// parse error, and a factory that throws. The stripExports transform in
// install.go rewrites every export line in the file on its way out, so "we
// generated it, it must be fine" is precisely the assumption that would put a
// broken file there.
//
// So a `--global --agent pi` install LOADS THE ARTIFACT ONCE, with the real pi,
// before it writes anything. Project scope is not gated this way on purpose: a
// project-local extension that fails breaks that project only, and pi prints
// the `-ne` hint that gets the user out of it.

// TestPiProbeVerdictReadsARealPiRun is the classifier, over output captured
// from pi 0.85.1 rather than invented. It is a pure function of those bytes, so
// every one of these cases runs on a machine with no pi installed.
func TestPiProbeVerdictReadsARealPiRun(t *testing.T) {
	const ext = "/tmp/wterm-pi-probe-123/agent/extensions/wterm.ts"

	// The sentinel has to be a provider NOBODY HAS, and the cases below cannot
	// see that: they build their fixtures out of the same constant, so changing
	// it to a real provider name moves both sides of every comparison at once.
	// What a real provider name would cost is the positive control -- pi would
	// get past the extensions and then fail somewhere else, or succeed, and the
	// probe would read either as "no answer" and refuse every global install.
	if !strings.Contains(piProbeProvider, "wterm-web") {
		t.Errorf("piProbeProvider = %q. It must be a name no provider will ever have, or the line the probe reads as its positive control is a line pi may never print", piProbeProvider)
	}

	for _, tc := range []struct {
		name string
		out  string
		want piVerdict
	}{
		// The positive control, and the reason the probe pins a provider that
		// cannot exist. This line is printed AFTER every extension has been
		// loaded and BEFORE anything reaches a network, so seeing it is proof
		// that pi got past extension loading -- which a bare exit code is not,
		// because pi exits 1 either way.
		{
			name: "the artifact loaded, and pi died on the probe's own bogus provider",
			out:  "Error: Unknown provider \"" + piProbeProvider + "\". Use --list-models to see available providers/models.\n",
			want: piLoads,
		},
		{
			name: "no default export",
			out: "Error: Failed to load extension \"" + ext + "\": Extension does not export a valid factory function: " + ext + "\n" +
				"Hint: Start without extensions using \"pi -ne\".\n",
			want: piRefuses,
		},
		{
			name: "a default export that is not a function",
			out: "Error: Failed to load extension \"" + ext + "\": Extension does not export a valid factory function: " + ext + "\n" +
				"Hint: Start without extensions using \"pi -ne\".\n",
			want: piRefuses,
		},
		{
			name: "a parse error",
			out:  "Error: Failed to load extension \"" + ext + "\": Failed to load extension: ParseError: Unexpected token\n",
			want: piRefuses,
		},
		{
			name: "a factory that throws",
			out:  "Error: Failed to load extension \"" + ext + "\": Failed to load extension: boom\n",
			want: piRefuses,
		},
		// The two inconclusive shapes, and they must NOT read as a pass. A
		// probe that cannot tell whether the artifact loaded has to say so:
		// "pi printed nothing I recognise" is not "pi is happy".
		{
			name: "pi died before it got anywhere near the extensions",
			out:  "Error: could not read settings\n",
			want: piUnknown,
		},
		{
			name: "pi printed nothing at all",
			out:  "",
			want: piUnknown,
		},
		// A failure naming SOMEBODY ELSE'S extension is not our artifact's
		// failure -- and cannot happen in the probe's own empty directory, but
		// a verdict that matched the message and ignored the path would call
		// every broken extension on the machine ours.
		// The mutant this is aimed at is matching the PATH alone. pi names
		// loaded extensions in its own startup output when it is not being
		// quiet -- and `quietStartup` is a setting, so which side of that a
		// probe lands on is not this command's to decide. A verdict built on
		// the path alone would read a perfectly good startup line as a refusal
		// and make the global install impossible to ever perform.
		{
			name: "the path named in a line that is not a failure",
			out: "Loaded extension " + ext + "\n" +
				"Error: Unknown provider \"" + piProbeProvider + "\". Use --list-models to see available providers/models.\n",
			want: piLoads,
		},
		{
			name: "a load failure naming a different file",
			out: "Error: Failed to load extension \"/home/someone/.pi/agent/extensions/other.ts\": Extension does not export a valid factory function\n" +
				"Error: Unknown provider \"" + piProbeProvider + "\".\n",
			want: piLoads,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := piProbeVerdict([]byte(tc.out), ext)
			if got != tc.want {
				t.Fatalf("verdict = %v, want %v (detail %q)", got, tc.want, detail)
			}
			// A refusal has to carry pi's own words out to the user: "pi
			// refused it" with no reason is not something anybody can act on.
			if got == piRefuses && !strings.Contains(detail, "Failed to load extension") {
				t.Errorf("the refusal detail does not carry pi's message: %q", detail)
			}
		})
	}
}

// TestPiGlobalInstallRefusesWhatPiRefuses. The verdict wired to the plan: a
// refusal writes nothing, and it happens in the planning phase, before the
// confirmation.
func TestPiGlobalInstallRefusesWhatPiRefuses(t *testing.T) {
	home := globalHome(t)
	stubPiProbe(t, piRefuses, `Failed to load extension "…/wterm.ts": Extension does not export a valid factory function`)

	// --yes, so that the refusal cannot be mistaken for the confirmation being
	// declined.
	r := installCLI(t, "y\n", "--agent", "pi", "--global", "--yes")

	r.wantCode(t, 1)
	if !strings.Contains(r.stderr, "does not export a valid factory function") {
		t.Errorf("the refusal does not carry pi's own message, which is the only actionable thing in it:\n%s", r)
	}
	if !strings.Contains(r.stderr, "every project") {
		t.Errorf("the refusal does not say why a global pi extension is the dangerous one:\n%s", r)
	}
	assertNothingUnder(t, home)
}

// TestPiGlobalInstallRefusesWhenItCannotValidate, which is the case with a
// judgement call in it, so the judgement is written down here.
//
// It REFUSES. The alternative -- install anyway with a warning -- trades a
// message the user may not read for a pi that will not start in any project on
// the machine, and the repair for that needs a user who knows both that pi has
// a `-ne` flag and which file to delete. The refusal names the project-scope
// install as the way through, because a project-local extension that fails
// breaks one project and pi prints the hint itself.
func TestPiGlobalInstallRefusesWhenItCannotValidate(t *testing.T) {
	home := globalHome(t)
	stubPiProbe(t, piUnknown, "pi is not on PATH")

	r := installCLI(t, "y\n", "--agent", "pi", "--global", "--yes")

	r.wantCode(t, 1)
	for _, want := range []string{"pi is not on PATH", "every project"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, r)
		}
	}
	// The way out has to be in the message, or the user's next move is to go
	// looking for a --force.
	if !strings.Contains(r.stderr, "install-integration --agent pi") {
		t.Errorf("the refusal does not name the project-scope install as the way through:\n%s", r)
	}
	assertNothingUnder(t, home)
}

// TestPiValidationRunsOnlyWhereItIsNeeded. Three cases that must NOT pay for a
// pi process, and the third is the one that matters: `--remove` deletes a file,
// and a `pi` that cannot be run is no reason to leave a broken extension in
// place.
func TestPiValidationRunsOnlyWhereItIsNeeded(t *testing.T) {
	home := globalHome(t)
	// Every probe from here on is a refusal, so anything that runs one fails.
	stubPiProbe(t, piRefuses, "this probe should not have run")

	t.Run("a project install does not validate", func(t *testing.T) {
		root := t.TempDir()
		installCLI(t, "", "--agent", "pi", "--yes", root).wantCode(t, 0)
	})

	t.Run("a global opencode install does not validate", func(t *testing.T) {
		installCLI(t, "", "--agent", "opencode", "--global", "--yes").wantCode(t, 0)
	})

	t.Run("a global remove does not validate", func(t *testing.T) {
		// Put one there without the probe's opinion, then take it away with it.
		stubPiProbe(t, piLoads, "")
		installCLI(t, "", "--agent", "pi", "--global", "--yes").wantCode(t, 0)
		stubPiProbe(t, piRefuses, "this probe should not have run")

		installCLI(t, "", "--agent", "pi", "--global", "--yes", "--remove").wantCode(t, 0)
		if _, err := os.Stat(filepath.Join(home, "pi-agent", "extensions", "wterm.ts")); err == nil {
			t.Errorf("--remove --global was blocked by a validation it does not need")
		}
	})
}

// TestARealPiLoadsTheArtifactThisInstallerWrites is the probe against the thing
// it exists to check, and it is the only test in this package that can tell you
// the answer is still true on pi 0.85.1. It SKIPS LOUDLY without pi: a
// validation gate whose only test is a table of canned strings is a table of
// canned strings.
func TestARealPiLoadsTheArtifactThisInstallerWrites(t *testing.T) {
	requirePi(t)

	artifact, err := renderIntegration(agents["pi"])
	if err != nil {
		t.Fatalf("rendering the pi artifact: %v", err)
	}

	t.Run("the artifact we ship", func(t *testing.T) {
		verdict, detail := runPiProbe(artifact)
		if verdict != piLoads {
			t.Fatalf("a real pi did not load the file this installer writes: verdict %v, %s", verdict, detail)
		}
	})

	// The other half, and the half that says the probe is looking at anything
	// at all: the four shapes pi rejects have to come back as refusals. Without
	// this, `return piLoads, ""` passes the test above.
	for _, tc := range []struct{ name, body string }{
		{"no default export", "export const handlers = 1\n"},
		{"a default export that is not a function", "export default 42\n"},
		{"a parse error", "export default function ( {\n"},
		{"a factory that throws", "export default function () { throw new Error('boom') }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, detail := runPiProbe([]byte(tc.body))
			if verdict != piRefuses {
				t.Fatalf("a real pi accepted %s: verdict %v, %s", tc.name, verdict, detail)
			}
		})
	}
}

// TestThePiProbeCarriesNothingOfTheUsersOwn is the environment the probe's pi
// is given, asserted directly.
//
// It is a unit test on a list of strings because the alternative -- run the
// probe and look for damage -- is VACUOUS, and was: an earlier version of this
// file ran the probe under a throwaway HOME and checked that the directory
// stayed empty, which a probe inheriting the real $HOME passes just as happily,
// because PI_CODING_AGENT_DIR is where pi puts auth.json and models-store.json
// and that one was still the probe's own. A test that a mutant walks through is
// not a test; the mutation run found this one.
func TestThePiProbeCarriesNothingOfTheUsersOwn(t *testing.T) {
	// A credential in the ambient environment, which is the thing that must not
	// travel. `pi --provider openai` with this set is exactly how a probe that
	// inherited its environment would end up making a request as the user.
	t.Setenv("OPENAI_API_KEY", "wterm-web-must-not-leak-this")
	t.Setenv("HOME", "/home/somebody")
	t.Setenv("PI_CODING_AGENT_DIR", "/home/somebody/.pi/agent")

	env := piProbeEnv("/tmp/probe/home", "/tmp/probe/agent")

	want := map[string]string{
		"HOME":                "/tmp/probe/home",
		"PI_CODING_AGENT_DIR": "/tmp/probe/agent",
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
		if k == "OPENAI_API_KEY" {
			t.Errorf("the probe's environment carries the user's credential: %s", kv)
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("probe env %s = %q, want %q -- an installer's pi must not be pointed at the user's own configuration", k, got[k], v)
		}
	}
	if got["PATH"] == "" {
		t.Errorf("the probe's environment has no PATH; pi is a node program and will not start")
	}
	// Nothing beyond the five. A probe built with append(os.Environ(), …) would
	// pass every assertion above and still hand pi the user's whole
	// environment.
	if len(env) != len(got) || len(env) != 5 {
		t.Errorf("the probe's environment holds %d entries: %q. It is built, not inherited", len(env), env)
	}
}

// TestThePiProbeRunsNowhereNearTheUsersOwnPi. The environment above, held
// against a real pi: nothing lands in the HOME this process was given.
func TestThePiProbeRunsNowhereNearTheUsersOwnPi(t *testing.T) {
	requirePi(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi-agent"))

	artifact, err := renderIntegration(agents["pi"])
	if err != nil {
		t.Fatalf("rendering the pi artifact: %v", err)
	}
	if verdict, detail := runPiProbe(artifact); verdict != piLoads {
		t.Fatalf("verdict %v, %s", verdict, detail)
	}

	// Nothing under the HOME this process was given, which is where pi would
	// have put auth.json and models-store.json if the probe had let it inherit
	// one. The probe builds its own.
	assertNothingUnder(t, home)
}

// TestTheIntegrationSourcesCarryTheSchemaTheInstallerWrites.
//
// The schema number is written down in four places -- Go's wtermSchema, the
// managed header of pi.ts and of opencode.js, and queue.ts's WTERM_SCHEMA,
// which is the number the two copies of the integration compare at run time to
// decide which of them reports. Four numbers that must agree and are typed out
// separately are four numbers that drift, and the drift is SILENT in the worst
// direction: a queue.ts left at 1 while the header says 2 makes a fresh global
// install lose the claim to a stale project one, forever, with no message
// anywhere.
func TestTheIntegrationSourcesCarryTheSchemaTheInstallerWrites(t *testing.T) {
	want := "wterm-schema: " + strconv.Itoa(wtermSchema)
	for _, name := range []string{"pi.ts", "opencode.js"} {
		body, err := integrations.File(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s's managed header does not say %q", name, want)
		}
	}

	queue, err := integrations.File("queue.ts")
	if err != nil {
		t.Fatalf("reading queue.ts: %v", err)
	}
	wantConst := "export const WTERM_SCHEMA = " + strconv.Itoa(wtermSchema)
	if !strings.Contains(string(queue), wantConst) {
		t.Errorf("queue.ts does not declare %q, so the two scopes would compare the wrong numbers", wantConst)
	}
}
