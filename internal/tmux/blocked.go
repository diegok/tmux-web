package tmux

import (
	"regexp"
	"strings"
)

// dialogRules is how one agent draws a prompt it is waiting on.
//
// One table per agent, so that adjusting the detector for a restyled dialog is
// a data change rather than a code change, and so that rules proven against one
// agent's screen are never applied to another's. Only Claude Code has been
// captured, so only Claude Code can be recognised: opencode and pi report
// working or idle and never blocked, which is exactly what v1 showed. That is
// the honest state of things, and it is preferable to guessing at a dialog
// nobody has seen.
type dialogRules struct {
	// rules are the runes an agent draws horizontal rules with. Claude Code
	// uses U+2500 for the solid rule above a tool call and U+254C for the
	// dashed ones inside a permission dialog. It draws no corners at all --
	// there is no closed box on screen, which is why the region is delimited by
	// rules rather than matched as a box.
	rules string
	// cursorChoice is the selection cursor sitting on a numbered option. This
	// is the load-bearing signal: it means a menu is on screen awaiting a
	// keystroke. Prose never carries it, and the bare cursor of the idle input
	// box is never followed by a number.
	cursorChoice *regexp.Regexp
	// choice is any numbered option, cursor or not. minChoices of them are
	// required, because a decision has alternatives; one line is a list item.
	//
	// Only numbered lines are counted, so a choice that wraps onto an
	// unnumbered continuation line -- Claude Code's second option routinely
	// does -- is counted once rather than twice. That wrapping is the hazard
	// question extraction has to handle; here it is simply invisible.
	choice     *regexp.Regexp
	minChoices int
	// question is the line that asks. Matching the phrasing ("Do you want")
	// would fit this detector to the single dialog anyone has captured; a line
	// that ends in a question mark is the general shape of an agent asking, and
	// combined with the cursor it is strict enough.
	question *regexp.Regexp
}

// blockedRules is the whole of the detector's knowledge, per agent.
//
// The footer -- "Esc to cancel · Tab to amend" -- is deliberately not required.
// It is the most style-volatile line on the screen and adds nothing over the
// cursor and the question, so requiring it would buy no strictness and cost
// every future restyle a missed badge.
var blockedRules = map[string]dialogRules{
	"claude": {
		rules:        "─╌",
		cursorChoice: regexp.MustCompile(`^❯ \d+\. `),
		choice:       regexp.MustCompile(`^(?:❯ )?\d+\. `),
		minChoices:   2,
		question:     regexp.MustCompile(`\?$`),
	},
}

// IsBlocked reports whether an agent's screen shows a prompt waiting on the
// user. It is deliberately strict and it never guesses: no match means the
// pane keeps whatever state churn decided, and a wrong "this one needs you"
// trains the owner to ignore the badge, which destroys the feature.
//
// The bottommost rule-delimited region is the one examined, because that is the
// live one. A dialog drawn above a fresh input box has already been answered.
// A screen with no rule on it at all has no dialog region and is never blocked.
func IsBlocked(agent, screen string) bool {
	r, ok := blockedRules[agent]
	if !ok {
		return false
	}

	lines := strings.Split(screen, "\n")
	last := -1
	for i, line := range lines {
		if isRule(line, r.rules) {
			last = i
		}
	}
	if last < 0 {
		return false
	}

	var choices int
	var cursor, asked bool
	for _, line := range lines[last+1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r.cursorChoice.MatchString(line) {
			cursor = true
		}
		if r.choice.MatchString(line) {
			choices++
		}
		if r.question.MatchString(line) {
			asked = true
		}
	}
	return cursor && asked && choices >= r.minChoices
}

// isRule reports whether a line is drawn entirely from an agent's rule runes.
func isRule(line, runes string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	return strings.Trim(line, runes) == ""
}
