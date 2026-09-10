package tmux

import (
	"regexp"
	"strings"
	"unicode/utf8"
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

	region, ok := dialogRegion(screen, r.rules)
	if !ok {
		return false
	}

	var choices int
	var cursor, asked bool
	for _, line := range region {
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

// dialogRegion returns the lines below the bottommost rule, and whether there
// was one.
//
// Shared by detection and extraction so that the two can never disagree about
// which dialog on the screen they are looking at -- a screen can hold an
// answered box above a live one, and reporting the state of the lower while
// quoting the text of the upper is worse than quoting nothing.
func dialogRegion(screen, rules string) ([]string, bool) {
	lines := strings.Split(screen, "\n")
	last := -1
	for i, line := range lines {
		if isRule(line, rules) {
			last = i
		}
	}
	if last < 0 {
		return nil, false
	}
	return lines[last+1:], true
}

// Question is the request a blocked agent is waiting on.
//
// It is present only when AgentState is blocked, and omitted entirely when
// extraction failed: the state is load-bearing and the text is a convenience,
// so a grammar that stops matching a restyled dialog must cost a quote in a
// tooltip rather than a badge.
type Question struct {
	Text    string   `json:"text"`
	Choices []string `json:"choices,omitempty"`
}

// ExtractQuestion returns the request on a blocked agent's screen, or nil if it
// cannot be read with confidence.
//
// It is meaningful only for a screen IsBlocked has already matched, and it
// deliberately does not re-check the markers IsBlocked checks -- the selection
// cursor in particular. Two copies of that rule would be two things to keep in
// step, and the caller has just run the authoritative one.
//
// Everything here fails closed: too few options, or no line asking anything
// above them, returns nil rather than half a dialog. The caller keeps the
// state it already decided.
func ExtractQuestion(agent, screen string) *Question {
	r, ok := blockedRules[agent]
	if !ok {
		return nil
	}
	region, ok := dialogRegion(screen, r.rules)
	if !ok {
		return nil
	}

	var choices []string
	first := -1
	// textCol is the column the current choice's own text starts at, or -1 when
	// no choice is open. A wrapped continuation is indented to line up under
	// it; the footer and the question sit hard against the left margin. That is
	// the difference between them, and it is the only one available -- matching
	// the footer's wording would tie extraction to the most style-volatile line
	// on the screen, which is exactly what the detector refuses to do.
	textCol := -1
	for i, raw := range region {
		line := strings.TrimRight(raw, " ")
		body := strings.TrimSpace(line)
		// A blank line needs no case of its own: its indent is 0, which is
		// below any choice's text column, so it falls through to the reset at
		// the bottom and closes the choice above it.
		indent := utf8.RuneCountInString(line) - utf8.RuneCountInString(strings.TrimLeft(line, " "))
		if prefix := r.choice.FindString(body); prefix != "" {
			if first < 0 {
				first = i
			}
			choices = append(choices, strings.TrimSpace(body[len(prefix):]))
			textCol = indent + utf8.RuneCountInString(prefix)
			continue
		}
		if textCol >= 0 && indent >= textCol {
			// Claude Code's second option routinely wraps onto a line that
			// carries no number. Joined with a single space: the wrap point is
			// a column, not a word break, so the two halves are one sentence.
			choices[len(choices)-1] = strings.TrimSpace(choices[len(choices)-1] + " " + body)
			continue
		}
		textCol = -1
	}

	// first < 0 is implied by the count today, since minChoices is 2. It is
	// checked anyway because the alternative is region[:-1] panicking in the
	// poller goroutine if anyone ever writes a rules table with minChoices 0,
	// and the whole point of that table is that it can be edited as data.
	if first < 0 || len(choices) < r.minChoices {
		return nil
	}

	// The question is above the options, because that is how a dialog reads and
	// because the region can hold other lines ending in "?" -- a diff preview
	// of a file that contains one, say. The nearest such line above the first
	// choice is the one being asked; IsBlocked does not care where it is,
	// because for a badge the existence of a question is the whole signal.
	text := ""
	for _, raw := range region[:first] {
		if body := strings.TrimSpace(raw); r.question.MatchString(body) {
			text = body
		}
	}
	if text == "" {
		return nil
	}
	return &Question{Text: text, Choices: choices}
}
