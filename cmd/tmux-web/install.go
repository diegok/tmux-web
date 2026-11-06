package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/diegok/tmux-web/internal/integrations"
)

// `tmux-web install-integration` writes the three agent integrations into a
// project, and it is A CLI ACT. There is no button for it in the web UI and
// there is not going to be one -- not even a "we detected claude, shall we…"
// prompt.
//
// The web UI is reachable over the network from a phone, and writing executable
// code into a repository is not a thing a network request should be able to do
// however well authenticated it is. The Origin middleware is the boundary for
// tmux operations; this is not a tmux operation. The structural half of that
// rule is free -- this file is in package main, which nothing can import -- and
// the other half, a reimplementation inside internal/front, is covered by a
// grep in install_test.go and by a comment where the routes are built.
//
// Three rules govern everything below:
//
//  1. A FILE WE OWN CARRIES THE MANAGED HEADER, and the schema line in it is
//     what makes a reinstall safe: ours-and-current is overwritten silently,
//     ours-but-older is overwritten with a word about it, and anything else is
//     refused by name. The last case is what the header exists for.
//  2. CLAUDE'S settings.json IS A FILE THE USER OWNS. It is merged into, never
//     rewritten: invalid JSON is refused rather than "fixed", only entries whose
//     command is recognisably ours are added or removed, and a merge that would
//     change nothing does not write at all.
//  3. NOTHING IS WRITTEN BEFORE THE PATHS ARE PRINTED AND THE USER HAS SAID
//     YES. Every check that can refuse runs in the planning phase, so a refusal
//     leaves the project exactly as it was.

// THE SCHEMA IS PER FILE (ownedFile.schema), and that is a decision about what
// the number means. It is the version in the managed header; bump it when the
// shape of what gets installed changes in a way a user should be told about, and
// the only thing it controls is whether an existing file of ours is replaced
// silently or with a note.
//
// One number for everything would tell an opencode user reinstalling that his
// file was replaced "because the shape changed" when nothing about his install
// differs except a digit, and a note that is false is worse than no note. That
// is why it is not global. It is not per AGENT either, and that is the newer
// half: pi owns two files that are versioned by two different things.
//
//   - opencodeSchema and piSchema are RUN-TIME numbers. Each integration
//     declares its own TMUX_WEB_SCHEMA -- pi.ts and opencode.js since the V2
//     port, queue.ts before that -- and two loaded copies of the SAME
//     integration compare their numbers to agree which of them reports.
//     Bumping one changes which installed copy of that integration wins.
//   - skillSchema versions one prose file, which claude and pi install the
//     same bytes of.
//
// Per AGENT, giving pi the skill forces piSchema to the skill's number, which
// would drag the integration's claim constant along -- so editing the prose of
// a claude skill would change which copy of the OPENCODE integration claims
// the reporter. Per file, the two move independently. Found by a pi reviewing
// this integration; docs/plans/2026-11-05-tmux-web-agent-control-followups.md
// has the chain written out.
//
// What used to be shared was opencode's and pi's pair: queue.ts, inlined into
// both, carried one TMUX_WEB_SCHEMA that the two scopes' copies compared at
// run time, and that single constant forced the two files' numbers to move
// together. The OpenCode V2 port split them: each integration now declares its
// own constant beside its header, because installs that change independently
// cannot share one number without one of them lying about it -- see queue.ts's
// claim block and pivalidate_test.go, which holds each file's constant to its
// own number.
const (
	claudeSchema   = 2 // claude's report wrapper
	skillSchema    = 2 // the skill, which claude and pi install from the same bytes
	opencodeSchema = 2 // opencode.js, with queue.ts inlined: the V2 plugin API and bus
	piSchema       = 1 // pi.ts, with the same queue.ts inlined
)

// managedHeader matches the header line every file this command writes carries
// near its top -- the FIRST line in the scripts and integrations, and
// deliberately the fifth in claude's skill, below the frontmatter, because a
// SKILL.md whose first line is not `---` has no frontmatter and would not load
// at all. ownerOf therefore searches the first 8 KiB rather than one line.
//
// It captures the schema NUMBER rather than matching a prefix, and that
// difference is the whole point: a prefix match cannot tell ours-and-current
// from ours-and-stale, which is two of the three cases the header exists to
// separate.
var managedHeader = regexp.MustCompile(`managed by tmux-web \(tmux-web-schema: (\d+)\)`)

// claudeScriptName is the wrapper's filename, and it is also how our hook
// entries are RECOGNISED in a settings.json we did not write alone. One
// constant for both so they cannot drift: matching by the hook's event name
// instead would make `--remove` delete a user's own PreToolUse entry, and
// matching by a hard-coded absolute path would make it delete nothing after the
// binary moved.
const claudeScriptName = "tmux-web-report.sh"

// queueImport is the line both integrations carry, and the one thing about them
// the installer has to understand. It is matched exactly; a near miss is an
// error at install time rather than a dangling import in somebody's project.
const queueImport = "import { claimReporter, makeQueue, spawnReport } from './queue.ts'\n"

// agents is what may be installed, and where each file goes.
//
// ALL THREE NOW OFFER --global, and the three mechanisms are different in ways
// this command has to know about. Measured on opencode 1.18.30 and pi 0.85.1:
//
//   - claude's hooks are documented to work in a user-level settings.json, and
//     that file is merged into rather than owned. It has always been here.
//   - opencode loads $XDG_CONFIG_HOME/opencode/plugin/ at global scope --
//     `opencode debug config` reports a file dropped there with
//     `"scope": "global"`. `plugins/` (plural) loads as well; the singular is
//     written because it is what the project install already uses, and one name
//     is one thing for a reader to keep straight.
//   - pi auto-loads $PI_CODING_AGENT_DIR/extensions/ with no settings entry and
//     no trust prompt, which makes it LESS gated than a project-local
//     extension, not more: that one needs `--approve`.
//
// Neither of the two new ones needs a user-owned file edited, and that is why
// they are here: the bar is rule 2 above, and a directory drop clears it in a
// way that a `plugin` array or a `pi install` does not. See globalSpec.note.
//
// ITS KEYS ARE tmux.Agents, and TestTheInstallableAgentsAreTheAgentsTheDaemonKnows
// holds them there. This is the fourth place the list of agents was written out
// and the only one that is not now derived from that one: an agent installable
// here and unknown to the daemon installs an integration whose every report is
// answered with `nothing known`, and an agent the daemon knows and this map does
// not is one `tmux-web install` refuses by name.
var agents = map[string]agentSpec{
	"pi": {
		// The extension MUST stay first: pivalidate_test.go renders files[0]
		// to hand a real pi the thing it ships.
		files: []ownedFile{
			{
				file:           filepath.Join(".pi", "extensions", "tmux-web.ts"),
				globalFile:     filepath.Join("extensions", "tmux-web.ts"),
				source:         "pi.ts",
				schema:         piSchema,
				globalValidate: validatePiArtifact,
			},
			{
				// MEASURED on pi 1.5.0, in a throwaway tmux with a HOME and a
				// PI_CODING_AGENT_DIR of its own: a skill at
				// <agent dir>/skills/<name>/SKILL.md loads (the startup screen
				// listed it under [Skills]), and a project one at
				// .pi/skills/<name>/SKILL.md loads once the project is trusted
				// -- untrusted, pi asks "Trust project folder?" and loads no
				// .pi resources at all. .agents/skills/ would work too and is
				// not used: it is shared with other agents and is searched up
				// the ancestors, so a drop there is not one agent's to own.
				//
				// The source is still named claude-skill.md. The bytes are the
				// same prose for both agents and the name is now wrong, but
				// renaming it moves the //go:embed line in
				// internal/integrations, which is where opencode's v2 port is
				// working; followups.md carries it.
				file:       filepath.Join(".pi", "skills", "tmux-web", "SKILL.md"),
				globalFile: filepath.Join("skills", "tmux-web", "SKILL.md"),
				source:     "claude-skill.md",
				schema:     skillSchema,
			},
		},
		global: &globalSpec{
			root: piAgentDir,
			note: "a global pi extension is auto-loaded in every project, with no entry in any settings file " +
				"and no trust prompt -- which is less gated than a project-local one, which needs `--approve`.\n" +
				"  tmux-web does not edit pi's own settings.json and does not run `pi install`: MEASURED on pi " +
				"0.85.1 and again on 1.3.0, `pi install` and `pi remove` keep every value and the key order " +
				"but reformat the whole file -- indentation normalised, arrays exploded one element per " +
				"line, the trailing newline " +
				"dropped. `pi remove` does not put it back either: it leaves its key behind as an empty " +
				"array. That is a wholesale rewrite of a file you own, which is the one thing this command " +
				"will not do. Uninstalling is deleting the files above",
		},
		// PROJECT SCOPE ONLY. MEASURED on pi 1.5.0: an untrusted project makes
		// pi ask "Trust project folder? ... This allows pi to load .pi
		// settings and resources, install missing project packages, and
		// execute project extensions", and until that is answered neither of
		// these files does anything. A global install has no such gate, which
		// is the asymmetry globalSpec.note is about.
		projectWarning: "pi loads neither of these until this project is trusted: answer pi's " +
			"`Trust project folder?` prompt once, or start it with `--approve` for a single run",
	},
	"opencode": {
		// The plugin MUST stay first: pivalidate_test.go holds files[0]'s
		// schema against the constant opencode.js declares, and the skill
		// carries a different number for a different reason.
		files: []ownedFile{
			{
				file:       filepath.Join(".opencode", "plugin", "tmux-web.js"),
				globalFile: filepath.Join("opencode", "plugin", "tmux-web.js"),
				source:     "opencode.js",
				schema:     opencodeSchema,
			},
			// The same prose claude and pi install, from the same bytes. Both
			// paths MEASURED on opencode 2.2.5 against opencode's own skill
			// list (`GET /api/skill?location[directory]=<dir>`, which needs no
			// model): each is listed, and when this global copy and a
			// ~/.claude/skills copy are both present the list carries ONE
			// tmux-web and it is this one. See
			// TestOpencodeOwnsItsSkillAtBothScopes.
			{
				file:       filepath.Join(".opencode", "skills", "tmux-web", "SKILL.md"),
				globalFile: filepath.Join("opencode", "skills", "tmux-web", "SKILL.md"),
				source:     "claude-skill.md",
				schema:     skillSchema,
			},
		},
		global: &globalSpec{
			root: opencodeConfigDir,
			note: "a global opencode plugin is loaded in every project, with no entry in any config file.\n" +
				"  tmux-web does not add a path to `plugin` in your opencode.jsonc: a directory drop is " +
				"uninstalled by deleting the one file above and leaves a file you own untouched.\n" +
				"  opencode also reads claude's `~/.claude/skills`, so a global claude install already " +
				"teaches it these verbs; the copy above takes precedence over that one and the agent is " +
				"never shown two (measured on opencode 2.2.5)",
		},
		// PROJECT SCOPE ONLY -- and EMPTY since the OpenCode V2 port. Through
		// V1 this warned that opencode's own bootstrap (package.json,
		// package-lock.json, node_modules/ and a .gitignore) landed in the
		// project the first time it loaded a plugin, which was MEASURED on
		// 1.18.30. Re-measured on 2.2.5, global and project scope both: a
		// dependency-free plugin leaves the project empty and puts nothing
		// beside itself in the configuration directory -- no bootstrap at all.
		// A warning about a repository the install does not touch teaches the
		// user to stop reading warnings, so there is no warning; if a future
		// opencode restores that footprint, the warning and its test come
		// back with it (TestInstallWarnsAboutOpencodesFootprint, retired).
		projectWarning: "",
	},
	"claude": {
		// The wrapper MUST stay first: the settings.json merge names it.
		//
		// The skill was the slot the other two agents would fill, and pi has
		// filled it: the entry below and pi's second file are the same source
		// and the same schema. It cost one entry in a slice, which is what
		// keeping it out of a claude-only code path bought.
		files: []ownedFile{
			{
				file:       filepath.Join(".claude", claudeScriptName),
				globalFile: filepath.Join(".claude", claudeScriptName),
				source:     "claude-report.sh",
				mode:       0o755,
				schema:     claudeSchema,
			},
			{
				file:       filepath.Join(".claude", "skills", "tmux-web", "SKILL.md"),
				globalFile: filepath.Join(".claude", "skills", "tmux-web", "SKILL.md"),
				source:     "claude-skill.md",
				schema:     skillSchema,
			},
		},
		// settings.json is not in `files`: it is the user's, and it is
		// merged rather than owned.
		settings: filepath.Join(".claude", "settings.json"),
		global: &globalSpec{
			root:     os.UserHomeDir,
			settings: filepath.Join(".claude", "settings.json"),
			note:     "claude reads hooks from your user-level settings.json as well as a project's, and this merges into it",
		},
	},
}

// agentSpec is one agent's installation.
type agentSpec struct {
	files          []ownedFile // every file we own for this agent, never empty
	settings       string      // a file the user owns that we merge into, if any
	global         *globalSpec // where --global puts it; nil would refuse it
	projectWarning string      // shown before the confirmation, PROJECT SCOPE ONLY
}

// ownedFile is one file we own for an agent.
type ownedFile struct {
	file       string      // relative to the project root
	globalFile string      // relative to the agent's global root
	source     string      // its name in internal/integrations' embed.FS
	mode       fs.FileMode // 0 means 0644
	// schema is what this file's managed header says; see claudeSchema for why
	// the number belongs to the file and not to the agent.
	schema int
	// globalValidate runs BEFORE anything is written and a non-nil error is a
	// refusal. Only pi's extension has one, and only at GLOBAL scope; see
	// pivalidate.go for why it is pi, and globalSpec.note for why global.
	//
	// It belongs to the FILE. On the scope it ran once per file of the agent,
	// which was correct only while pi owned exactly one: with two, pi's own
	// pre-flight would be handed the Markdown skill, a real pi would refuse
	// it, and every global pi install would refuse over a good skill. `rel` is
	// the file's path inside the agent's own directory, so that what gets
	// tried is tried where the agent would read it.
	globalValidate func(dir, rel string, artifact []byte) error
}

// globalSpec is where `--global` puts one agent's integration.
//
// It is a separate struct rather than four more fields on agentSpec because
// every one of them is a DIFFERENT ANSWER at the two scopes, and the two that
// are silently different are the dangerous ones: the directory (a project's
// .opencode/ against opencode's own configuration directory) and the warning (a
// repository's footprint against no repository at all).
type globalSpec struct {
	root     func() (string, error) // the agent's own configuration directory
	settings string                 // a file the user owns that we merge into, if any
	note     string                 // what this scope is, and what it does not touch
}

// -- the agents' own configuration directories -------------------------------

// opencodeConfigDir is where opencode keeps its global plugin directory.
//
// XDG_CONFIG_HOME IS HONOURED, measured with a clean HOME: the global plugin
// was loaded from $XDG_CONFIG_HOME/opencode/plugin/. Hard-coding ~/.config gets
// that wrong for everybody who sets it.
//
// A RELATIVE value is refused rather than resolved, and that is measured too:
// opencode 1.18.30 resolves a relative XDG_CONFIG_HOME against ITS OWN working
// directory and calls what it finds there `"scope": "local"` -- and while it is
// set, ~/.config/opencode/plugin/ is not read at all. So there is no directory
// this command could write to that would be global: which one opencode reads
// depends on where the user starts it. Guessing is how an integration ends up
// somewhere nothing reads.
func opencodeConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("XDG_CONFIG_HOME is set to %q, which is not an absolute path.\n"+
				"  opencode resolves a relative one against its own working directory and treats what it finds\n"+
				"  there as a LOCAL plugin, so there is no directory this command could call global. Set\n"+
				"  XDG_CONFIG_HOME to an absolute path, or install into one project instead", display(dir, 64))
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot work out where your home directory is: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}

// piAgentDir is pi's own configuration directory. PI_CODING_AGENT_DIR is what
// pi reads; the default is ~/.pi/agent, measured by running pi under a clean
// HOME and watching it create ~/.pi/agent/auth.json.
//
// A RELATIVE value is refused rather than resolved, for the same reason and in
// the same shape as opencodeConfigDir above -- and the two being different was
// a bug, found by a pi reviewing this integration. MEASURED TWICE on pi 1.5.0,
// independently: started from two different directories with the same relative
// PI_CODING_AGENT_DIR, pi created <cwd>/relconf under each of them and nothing
// under HOME. Resolving it here instead resolves it against the INSTALLER's
// working directory, which writes an extension that no pi will read.
func piAgentDir() (string, error) {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("PI_CODING_AGENT_DIR is set to %q, which is not an absolute path.\n"+
				"  pi resolves a relative one against its own working directory, so which directory it\n"+
				"  reads depends on where you start it and there is none this command could call global.\n"+
				"  Set PI_CODING_AGENT_DIR to an absolute path, or install into one project instead", display(dir, 64))
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot work out where your home directory is: %w", err)
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// -- the command ------------------------------------------------------------

func cmdInstall(args []string, stdout, stderr io.Writer) int {
	// os.Stdin at the boundary and an io.Reader inside, the same shape
	// cmdReport uses: the confirmation is the only thing here that reads one,
	// and a test has to be able to answer it.
	return runInstall(args, os.Stdin, stdout, stderr)
}

func runInstall(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := newFlagSet("install-integration", stderr,
		"tmux-web install-integration --agent claude|opencode|pi [dir] [--global] [--yes] [--remove]")
	agent := fset.String("agent", "", "which agent to install for: claude, opencode or pi")
	global := fset.Bool("global", false, "install into the user's own configuration instead of a project")
	yes := fset.Bool("yes", false, "do not ask for confirmation")
	remove := fset.Bool("remove", false, "remove what an install wrote, and nothing else")
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}

	spec, known := agents[*agent]
	if !known {
		return usageError(stderr, fset, "--agent must be one of claude, opencode or pi; got %q", display(*agent, 32))
	}
	if len(operands) > 1 {
		return usageError(stderr, fset, "install-integration takes at most one directory, got %d", len(operands))
	}
	if *global && len(operands) > 0 {
		return usageError(stderr, fset, "--global installs into your own configuration; it takes no directory, got %q", operands[0])
	}

	where, err := resolveScope(spec, *global, operands)
	if err != nil {
		return fail(stderr, err)
	}

	plan, err := planInstall(*agent, spec, where, *remove)
	if err != nil {
		return fail(stderr, err)
	}
	plan.describe(stdout, stderr)
	if plan.empty() {
		return 0
	}
	if !*yes && !confirm(stdin, stderr) {
		fmt.Fprintln(stderr, "nothing was written")
		return 1
	}
	if err := plan.apply(stderr); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// scope is one resolved installation: which files this run is about, and what
// the user has to be told about the place they are going.
//
// Everything downstream reads these fields rather than re-deciding from a
// `global` boolean, because the two scopes differ in more than a directory --
// they differ in whether there is a settings file, whether there is a
// pre-flight, whether an ancestor project is worth looking for, and what
// warning is true.
type scope struct {
	global   bool
	files    []scopedFile // the files we own, absolute, in the spec's order
	settings string       // a file the user owns that we merge into, absolute; "" if none
	dir      string       // the directory the first file lives in, for messages
	note     string       // what this scope is
	warning  string       // what it costs
}

// scopedFile is one ownedFile placed in a scope.
type scopedFile struct {
	path   string
	source string
	mode   fs.FileMode
	schema int
	// validate is the file's globalValidate with its path already bound, and
	// it is nil at project scope -- which is the gate that keeps a project
	// install from starting a real pi.
	validate func(dir string, artifact []byte) error
}

// bindValidate ties one file's global pre-flight to the path it will have
// inside the agent's own directory. resolveScope is the only place that knows
// both, so it is the only place that can.
func bindValidate(f ownedFile) func(string, []byte) error {
	if f.globalValidate == nil {
		return nil
	}
	rel, v := f.globalFile, f.globalValidate
	return func(dir string, artifact []byte) error { return v(dir, rel, artifact) }
}

// resolveScope works out that. Every path is made absolute, because every path
// this command prints is one the user may have to find later from somewhere
// else.
func resolveScope(spec agentSpec, global bool, operands []string) (scope, error) {
	if global {
		g := spec.global
		root, err := g.root()
		if err != nil {
			return scope{}, err
		}
		out := scope{global: true, note: g.note}
		for _, f := range spec.files {
			out.files = append(out.files, scopedFile{
				path:     filepath.Join(root, f.globalFile),
				source:   f.source,
				mode:     f.mode,
				schema:   f.schema,
				validate: bindValidate(f),
			})
		}
		if g.settings != "" {
			out.settings = filepath.Join(root, g.settings)
		}
		out.dir = filepath.Dir(out.files[0].path)
		return out, nil
	}

	dir := "."
	if len(operands) == 1 {
		dir = operands[0]
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return scope{}, fmt.Errorf("cannot resolve %q: %w", dir, err)
	}
	if info, err := os.Stat(root); err != nil {
		return scope{}, fmt.Errorf("%s: %w", root, err)
	} else if !info.IsDir() {
		return scope{}, fmt.Errorf("%s is not a directory", root)
	}
	out := scope{warning: spec.projectWarning, dir: root}
	for _, f := range spec.files {
		out.files = append(out.files, scopedFile{
			path:   filepath.Join(root, f.file),
			source: f.source,
			mode:   f.mode,
			schema: f.schema,
		})
	}
	if spec.settings != "" {
		out.settings = filepath.Join(root, spec.settings)
	}
	return out, nil
}

// -- the plan ---------------------------------------------------------------

// installPlan is everything a run will do, worked out before it does any of it.
//
// Building it is where every refusal happens -- a file we do not own, a
// settings.json that is not JSON -- so that a refused run has written nothing
// and a confirmed run is very unlikely to fail half way.
type installPlan struct {
	agent    string
	remove   bool
	files    []plannedFile
	settings *plannedSettings
	// notes and warnings are both printed before the confirmation and are
	// different things. A note says what this scope IS -- where the file goes,
	// what is deliberately not being edited. A warning says what it COSTS.
	notes    []string
	warnings []string
}

// plannedFile is one file we own, already rendered.
type plannedFile struct {
	path    string
	content []byte // nil when removing
	mode    fs.FileMode
	note    string // what the user is told about this path
}

// plannedSettings is the merge into a file the user owns.
type plannedSettings struct {
	path    string
	content []byte // nil when the merge changes nothing
	note    string
}

func planInstall(agent string, spec agentSpec, where scope, remove bool) (*installPlan, error) {
	plan := &installPlan{agent: agent, remove: remove}

	// Every file is checked before any is planned: one foreign file among
	// several refuses the whole run, so a refusal still writes nothing.
	type found struct {
		state  ownership
		schema int
	}
	states := make([]found, len(where.files))
	present := 0
	for i, f := range where.files {
		state, schema, err := ownerOf(f.path, f.schema)
		if err != nil {
			return nil, err
		}
		if state == ownForeign {
			return nil, fmt.Errorf("%s exists and tmux-web did not write it (it carries no `managed by tmux-web` header).\n"+
				"  refusing to touch it -- move it aside if you want this integration installed there", f.path)
		}
		states[i] = found{state, schema}
		if state != ownAbsent {
			present++
		}
	}
	// The first file stands for the agent where a single path is wanted: it is
	// the one settings.json names.
	path := where.files[0].path

	switch {
	case remove:
		// One warning, and only when NOTHING is there: a claude installed
		// before the skill existed has the wrapper and not the skill, and
		// being told the skill is missing would be noise about a file that
		// was never ours to remove.
		if present == 0 {
			plan.warnings = append(plan.warnings, "there is no tmux-web integration at "+path)
		}
		for i, f := range where.files {
			if states[i].state != ownAbsent {
				plan.files = append(plan.files, plannedFile{path: f.path, note: "remove"})
			}
		}
	default:
		for i, f := range where.files {
			content, err := renderOwned(f.source)
			if err != nil {
				return nil, err
			}
			// THE PRE-FLIGHT GOES HERE, in the planning phase, with everything
			// else that can refuse: a refused run has written nothing. It runs
			// on install only -- `--remove` deletes a file, and a `pi` that
			// cannot be run is no reason to leave a broken extension in place.
			if f.validate != nil {
				if err := f.validate(where.dir, content); err != nil {
					return nil, err
				}
			}
			mode := f.mode
			if mode == 0 {
				mode = 0o644
			}
			note := "new"
			switch states[i].state {
			case ownCurrent:
				note = "replacing ours"
			case ownOutdated:
				note = fmt.Sprintf("replacing ours, written by an older tmux-web (tmux-web-schema: %d)", states[i].schema)
			}
			plan.files = append(plan.files, plannedFile{path: f.path, content: content, mode: mode, note: note})
		}
		if where.note != "" {
			plan.notes = append(plan.notes, where.note)
		}
		if where.warning != "" {
			plan.warnings = append(plan.warnings, where.warning)
		}
		if where.global {
			// The other half of the duplicate-load story, from the global side.
			// Neither runtime dedupes by filename, so both copies load in one
			// process; queue.ts's claim makes that harmless -- one copy
			// reports, the newer schema wins -- but harmless is not intended.
			plan.notes = append(plan.notes,
				"this loads in EVERY project. A project that also carries a tmux-web integration for "+agent+
					" loads both copies in one process; they agree at run time on one of them doing the reporting, "+
					"and the newer of the two wins")
		} else {
			if nested := nestedInstall(where.dir); nested != "" {
				// Open question 8, and the instruction is to warn where it can
				// be detected and not to attempt a resolution: two integrations
				// writing one pane option is a race nobody has looked at.
				plan.warnings = append(plan.warnings,
					"an ancestor of this directory already carries a tmux-web integration:\n    "+nested+
						"\n  if an agent run here loads both, two integrations write the same pane option. Nobody has measured that race")
			}
			// And the same thing seen from the project side, which is the side
			// the user can act on: they have just been given two copies.
			if g := globalInstall(spec); g != "" {
				plan.notes = append(plan.notes,
					"this agent already has a tmux-web integration installed globally:\n    "+g+
						"\n  both copies load in one process. They agree at run time on one of them doing the "+
						"reporting, and the newer of the two wins -- but `--global --remove` is how you get back to one")
			}
		}
	}

	if where.settings != "" {
		merged, err := planSettings(where.settings, path, remove)
		if err != nil {
			return nil, err
		}
		plan.settings = merged
	}
	return plan, nil
}

// globalInstall is the global path for one agent, if one of ours is sitting
// there. It answers nothing when the directory cannot even be worked out: an
// unset home is not a reason to fail a project install.
func globalInstall(spec agentSpec) string {
	if spec.global == nil {
		return ""
	}
	root, err := spec.global.root()
	if err != nil {
		return ""
	}
	for _, f := range spec.files {
		path := filepath.Join(root, f.globalFile)
		if state, _, err := ownerOf(path, f.schema); err == nil && (state == ownCurrent || state == ownOutdated) {
			return path
		}
	}
	return ""
}

// planSettings works out the merge into claude's settings.json.
func planSettings(path, script string, remove bool) (*plannedSettings, error) {
	original, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	updated, changed, err := mergeClaudeHooks(original, script, remove)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return &plannedSettings{path: path, note: "no change"}, nil
	}
	note := "merging in " + strconv.Itoa(len(integrations.ClaudeHookEvents)) + " hooks"
	if remove {
		note = "removing our hooks"
	}
	return &plannedSettings{path: path, content: updated, note: note}, nil
}

func (p *installPlan) empty() bool {
	return len(p.files) == 0 && (p.settings == nil || p.settings.content == nil)
}

// describe prints the exact paths on stdout -- they are the answer, and they
// are what the user is being asked to approve -- and the commentary on stderr.
func (p *installPlan) describe(stdout, stderr io.Writer) {
	verb := "will be written"
	if p.remove {
		verb = "will be removed"
	}
	if p.empty() {
		fmt.Fprintf(stderr, "nothing to do for %s\n", p.agent)
	} else {
		fmt.Fprintf(stderr, "these paths %s:\n", verb)
	}
	for _, f := range p.files {
		fmt.Fprintf(stdout, "%s\n", f.path)
		fmt.Fprintf(stderr, "  %s  (%s)\n", f.path, f.note)
	}
	if s := p.settings; s != nil {
		if s.content != nil {
			fmt.Fprintf(stdout, "%s\n", s.path)
		}
		fmt.Fprintf(stderr, "  %s  (%s)\n", s.path, s.note)
	}
	for _, n := range p.notes {
		fmt.Fprintf(stderr, "note: %s\n", n)
	}
	for _, w := range p.warnings {
		fmt.Fprintf(stderr, "note: %s\n", w)
	}
}

// apply does what describe printed, and nothing else.
func (p *installPlan) apply(stderr io.Writer) error {
	for _, f := range p.files {
		if f.content == nil {
			if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("removing %s: %w", f.path, err)
			}
			fmt.Fprintf(stderr, "removed %s\n", f.path)
			continue
		}
		if err := writeFileAtomically(f.path, f.content, f.mode); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "wrote %s\n", f.path)
	}
	if s := p.settings; s != nil && s.content != nil {
		if err := writeFileAtomically(s.path, s.content, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "merged %s\n", s.path)
	}
	return nil
}

// writeFileAtomically writes through a temporary file in the same directory, so
// that an interrupted run cannot leave a truncated settings.json -- which is
// the user's file, and the one thing here that cannot be regenerated.
func writeFileAtomically(path string, content []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// The mode is set on the temporary file and carried over by the rename.
	// CreateTemp makes it 0600, and for claude-report.sh that would be a hook
	// that exits 126 -- which, because every hook is registered "async": true,
	// Claude ignores. A non-executable wrapper is not an error message
	// anywhere; it is a pane that silently stops being reported on.
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// confirm asks, on stderr, and treats anything that is not a yes as a no --
// including an EOF, which is what a piped-in empty stdin looks like.
func confirm(stdin io.Reader, stderr io.Writer) bool {
	fmt.Fprint(stderr, "continue? [y/N] ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// -- ownership --------------------------------------------------------------

type ownership int

const (
	ownAbsent ownership = iota
	ownCurrent
	ownOutdated
	ownForeign
)

// ownerOf reads the managed header out of a file's first few kilobytes.
func ownerOf(path string, current int) (ownership, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ownAbsent, 0, nil
	}
	if err != nil {
		return ownForeign, 0, fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()

	head := make([]byte, 8<<10)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ownForeign, 0, fmt.Errorf("%s: %w", path, err)
	}
	m := managedHeader.FindSubmatch(head[:n])
	if m == nil {
		return ownForeign, 0, nil
	}
	schema, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return ownForeign, 0, nil
	}
	if schema == current {
		return ownCurrent, schema, nil
	}
	return ownOutdated, schema, nil
}

// nestedInstall looks up the directory tree for another of our integrations and
// returns the first one it finds. It does not resolve anything: open question 8
// is open, and a guess about which of two integrations should win is exactly
// the kind of plausible reasoning this project keeps having to unpick.
func nestedInstall(root string) string {
	dir := filepath.Dir(root)
	for {
		for _, spec := range agents {
			for _, f := range spec.files {
				candidate := filepath.Join(dir, f.file)
				if state, _, err := ownerOf(candidate, f.schema); err == nil && (state == ownCurrent || state == ownOutdated) {
					return candidate
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// -- rendering --------------------------------------------------------------

// renderOwned turns one owned file's embedded bytes into the file that gets
// written, named by its source rather than by the agent that owns it -- because
// claude owns two of them, and only the agent's spec knows which is which.
//
// For claude's wrapper that is the script with its BIN resolved; for its skill
// it is the prose unchanged; for pi and opencode it is the integration with
// queue.ts inlined and every export but the default stripped. Every transform
// happens HERE, in Go, from one embedded copy -- there is never a second copy
// of the queue in the repository.
func renderOwned(source string) ([]byte, error) {
	switch source {
	case "claude-report.sh":
		return integrations.ClaudeReportScript(resolveBinary()), nil
	case "claude-skill.md":
		// Written as embedded: a skill is prose, and nothing in it is only
		// known at install time.
		b, err := integrations.File(source)
		if err != nil {
			return nil, fmt.Errorf("reading the embedded %s: %w", source, err)
		}
		return b, nil
	}
	src, err := integrations.File(source)
	if err != nil {
		return nil, fmt.Errorf("reading the embedded %s: %w", source, err)
	}
	queue, err := integrations.File("queue.ts")
	if err != nil {
		return nil, fmt.Errorf("reading the embedded queue.ts: %w", err)
	}
	return inlineQueue(src, queue)
}

// inlineQueue makes one self-contained file out of two, and the reason it is
// one file is MEASURED rather than tidy. Both runtimes treat the integration's
// directory as a directory of plugins, not as a module tree:
//
//   - opencode 1.18.30 loads EVERY file in .opencode/plugin/ and calls EVERY
//     EXPORTED FUNCTION of each one as a plugin factory -- the default export
//     and the named ones alike. A queue module written beside the plugin is
//     therefore a second plugin, and the plugin's own `handlers` export is a
//     third: called with opencode's plugin input where it expects a `report`
//     function, it returns a hooks object that throws on the first event.
//     Measured with the real file, in a throwaway project with a recording stub
//     on PATH: shipped verbatim, one malformed report and the rest of the
//     turn's reports gone; with the named exports stripped, chat.message and
//     session.status(busy) both arrived intact.
//   - pi 0.85.1 REFUSES TO START when a file in .pi/extensions/ exports no
//     factory: "Failed to load extension ".../tmux-web-queue.ts": Extension does
//     not export a valid factory function", plus a hint to restart with -ne.
//
// So: no sibling module, and no export the runtime can mistake for an entry
// point. The `export default` is kept because it IS the entry point.
func inlineQueue(src, queue []byte) ([]byte, error) {
	if !bytes.Contains(src, []byte(queueImport)) {
		return nil, fmt.Errorf("the embedded integration no longer imports the queue as %q; "+
			"install.go inlines that import and cannot guess at a new one", strings.TrimSpace(queueImport))
	}
	out := bytes.Replace(src, []byte(queueImport), append([]byte(inlineBanner), queue...), 1)
	out = stripExports(out)
	// The postcondition, asserted rather than assumed: exactly one entry point
	// and nothing else the runtime will call. It is checked AFTER the
	// concatenation rather than on each half, because one pass over the joined
	// file is one thing a test can kill -- and because the interesting failure
	// is not "queue.ts exports something", it is "the file we are about to
	// write exports something opencode will call".
	exports := topLevelExports(out)
	if len(exports) != 1 || !strings.HasPrefix(exports[0], "export default") {
		return nil, fmt.Errorf("the installed file would carry these top-level exports: %q; want exactly one `export default`", exports)
	}
	return out, nil
}

// inlineBanner replaces the import line, so that the file says what happened to
// it where the import used to be.
const inlineBanner = `// -- queue.ts, inlined by ` + "`tmux-web install-integration`" + ` -----------------
//
// It is inlined rather than written beside this file, and every export but the
// entry point below is stripped, because both runtimes treat this directory as
// a directory of PLUGINS rather than as a module tree: opencode calls every
// exported function of every file in it as a plugin factory, and pi refuses to
// start when a file in it exports no factory at all. The single source of truth
// is internal/integrations/queue.ts in tmux-web; this copy is generated.
`

// topLevelExport matches an export declaration at the start of a line, which in
// these two files is the only place one can be.
var topLevelExport = regexp.MustCompile(`(?m)^export .*$`)

// stripExports removes the `export` keyword from every top-level declaration
// but the default one, which is the runtime's entry point and the only thing in
// the file it is meant to call.
func stripExports(src []byte) []byte {
	return topLevelExport.ReplaceAllFunc(src, func(line []byte) []byte {
		if bytes.HasPrefix(line, []byte("export default")) {
			return line
		}
		return bytes.TrimPrefix(line, []byte("export "))
	})
}

// topLevelExports lists what a rendered file still exports, for the assertions
// above.
func topLevelExports(src []byte) []string {
	var out []string
	for _, m := range topLevelExport.FindAll(src, -1) {
		out = append(out, string(m))
	}
	return out
}

// resolveBinary is the path written into the claude wrapper.
//
// Our own path first, because the tmux-web being run is the tmux-web the user
// means; a PATH lookup as the fallback. If neither works the bare name is
// written, and the wrapper's own `[ -x "$BIN" ]` guard turns that into silence
// rather than into an error on every tool call.
func resolveBinary() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved
		}
		return exe
	}
	if p, err := exec.LookPath("tmux-web"); err == nil {
		return p
	}
	return "tmux-web"
}

// -- claude's settings.json -------------------------------------------------

// mergeClaudeHooks adds or removes our hook entries in the user's settings.
//
// Everything it does not understand is carried across as json.RawMessage: every
// top-level key but "hooks", every hook event but the four we register, and
// every entry in those four that is not ours. Those bytes come back out exactly
// as they went in. What is NOT preserved is key order and whitespace, because
// encoding/json has no way to preserve them -- promising the file back
// byte-for-byte means writing a format-preserving JSON editor, which is not
// scoped here. What bounds the damage instead is this function's own rules,
// plus the one below it: a merge that changes nothing does not write.
func mergeClaudeHooks(original []byte, script string, remove bool) ([]byte, bool, error) {
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(original)) > 0 {
		if err := json.Unmarshal(original, &top); err != nil {
			return nil, false, fmt.Errorf("this is your file and it is not valid JSON, so tmux-web will not "+
				"rewrite it or try to fix it: %w", err)
		}
	}

	hooks := map[string]json.RawMessage{}
	if raw, ok := top["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, false, fmt.Errorf(`"hooks" is not an object of event names, which is the only shape `+
				`this command knows how to merge into: %w`, err)
		}
	}

	desired, err := desiredClaudeEntries(script)
	if err != nil {
		return nil, false, err
	}

	changed := false
	placed := map[string]bool{}
	// Every event is swept, not just the four we register: an install by an
	// older tmux-web may have left an entry somewhere this one no longer
	// writes, and `--remove` that only looked at today's four would leave it
	// running.
	//
	// Ours is replaced IN PLACE rather than dropped and re-appended, and that
	// is what makes a reinstall a no-op instead of a reordering. A reordering
	// would be semantically identical and would still rewrite the user's file
	// on every run, which is the wholesale-rewrite mutant wearing a hat.
	for event, raw := range hooks {
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, false, fmt.Errorf("hooks.%s is not a list of hook entries: %w", event, err)
		}
		eventChanged := false
		kept := make([]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			if !isOurEntry(entry) {
				kept = append(kept, entry)
				continue
			}
			want, wanted := desired[event]
			if remove || !wanted || placed[event] {
				// Removing, or an entry of ours for an event this version no
				// longer registers, or a second copy of ours in one list.
				eventChanged = true
				continue
			}
			placed[event] = true
			if !sameJSON(entry, want) {
				eventChanged = true
			}
			kept = append(kept, want)
		}
		if !eventChanged {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		merged, err := json.Marshal(kept)
		if err != nil {
			return nil, false, err
		}
		hooks[event] = merged
	}

	if !remove {
		for _, event := range integrations.ClaudeHookEvents {
			if placed[event] {
				continue
			}
			var entries []json.RawMessage
			if raw, ok := hooks[event]; ok {
				if err := json.Unmarshal(raw, &entries); err != nil {
					return nil, false, fmt.Errorf("hooks.%s is not a list of hook entries: %w", event, err)
				}
			}
			merged, err := json.Marshal(append(entries, desired[event]))
			if err != nil {
				return nil, false, err
			}
			hooks[event] = merged
			changed = true
		}
	}

	if !changed {
		return nil, false, nil
	}
	if len(hooks) == 0 {
		delete(top, "hooks")
	} else {
		merged, err := json.Marshal(hooks)
		if err != nil {
			return nil, false, err
		}
		top["hooks"] = merged
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The user's own strings go back unescaped: Go's default would turn a `<`
	// or an `&` inside one of THEIR values into \u003c, which is a change to
	// their file that this command has no business making.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(top); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

// desiredClaudeEntries is the block Task 19 generates, taken apart into one
// matcher entry per event.
//
// The shape is read back out of internal/integrations rather than rebuilt here:
// the `"async": true` that keeps the hook off the turn's critical path, the
// absence of `asyncRewake`, and the closed set of four events are that
// package's decisions, tested there, and a second declaration of the same shape
// in this file would be a second thing to keep in step.
func desiredClaudeEntries(script string) (map[string]json.RawMessage, error) {
	var block struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(integrations.ClaudeHookBlock(script), &block); err != nil {
		return nil, fmt.Errorf("the generated hook block is not the JSON this command expects: %w", err)
	}
	out := make(map[string]json.RawMessage, len(block.Hooks))
	for _, event := range integrations.ClaudeHookEvents {
		entries, ok := block.Hooks[event]
		if !ok || len(entries) != 1 {
			return nil, fmt.Errorf("the generated hook block has %d entries for %s, want one", len(entries), event)
		}
		out[event] = entries[0]
	}
	if len(block.Hooks) != len(out) {
		return nil, fmt.Errorf("the generated hook block registers %d events, and integrations.ClaudeHookEvents names %d",
			len(block.Hooks), len(out))
	}
	return out, nil
}

// sameJSON compares two encodings of the same value. It is compacted on both
// sides because the entry standing in the user's file carries that file's
// indentation and the generated one carries the generator's, and neither is a
// difference worth rewriting a file over.
func sameJSON(a, b json.RawMessage) bool {
	var x, y bytes.Buffer
	if err := json.Compact(&x, a); err != nil {
		return false
	}
	if err := json.Compact(&y, b); err != nil {
		return false
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}

// isOurEntry decides whether one matcher entry in settings.json was written by
// this command.
//
// BY THE COMMAND, not by the event name. A user's own PreToolUse entry lives in
// the same list as ours, and an uninstall that emptied the list would take
// theirs with it. Anything that does not parse as a hook entry is somebody
// else's by definition and is left alone.
func isOurEntry(entry json.RawMessage) bool {
	var matcher struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(entry, &matcher); err != nil {
		return false
	}
	for _, h := range matcher.Hooks {
		if strings.Contains(h.Command, claudeScriptName) {
			return true
		}
	}
	return false
}
