package tmux

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// dialog is how one agent draws a prompt it is waiting on, and how to read it.
//
// One implementation per agent, so that adjusting the detector for a restyled
// dialog is a data change rather than a code change, and so that rules proven
// against one agent's screen are never applied to another's.
//
// It is an interface rather than one table of fields because the three captured
// screens share no structure at all. Claude Code draws a region delimited by
// horizontal rules, holding numbered choices under a line that ends in a
// question mark, and needs all three signals together to be strict. opencode
// draws a left-guttered block whose first line says "Permission required"
// outright. pi has no permission dialog: it asks through a tool, which renders
// a closed box holding a two-pane selector. Measured over the three captures,
// no two share a structural signal:
//
//	          corners  rules ─  gutter ┃  box │  numbered
//	claude          0      100          0       0          3
//	opencode        0        0         18       0          0
//	pi              4      319          0      60          4
//
// Bending any of them through machinery written for another would mean
// loosening that machinery until it fit -- and a rule broad enough to cover all
// three shapes is broad enough to fire on prose.
type dialog interface {
	// isBlocked reports whether this screen shows a prompt waiting on the user.
	isBlocked(screen string) bool
	// extractQuestion returns the request being waited on, or nil when it
	// cannot be read with confidence.
	extractQuestion(screen string) *Question
}

// blockedRules is the whole of the detector's knowledge, per agent.
//
// One entry per agent whose screen has been captured. An agent with no entry --
// any command on the Agents list that nobody has recorded asking a question --
// reports working or idle and is never blocked, which is preferable to guessing
// at a dialog nobody has seen. The day one is captured, the entry is a data
// change.
var blockedRules = map[string]dialog{
	"claude": claudeDialog{
		rules:        "─╌",
		cursorChoice: regexp.MustCompile(`^❯ \d+\. `),
		choice:       regexp.MustCompile(`^(?:❯ )?\d+\. `),
		minChoices:   2,
		question:     regexp.MustCompile(`\?$`),
	},
	"opencode": opencodeDialog{
		gutter:  "┃",
		header:  regexp.MustCompile(`^\W*Permission required$`),
		request: regexp.MustCompile(`^→\s+(\S.*)$`),
	},
	"pi": piDialog{
		top:          regexp.MustCompile(`╭.*╮`),
		bottom:       regexp.MustCompile(`╰.*╯`),
		wall:         "│",
		cursorChoice: regexp.MustCompile(`^→ \d+\. `),
		choice:       regexp.MustCompile(`^(?:→ )?\d+\. `),
		minChoices:   2,
	},
}

// IsBlocked reports whether an agent's screen shows a prompt waiting on the
// user. It is deliberately strict and it never guesses: no match means the
// pane keeps whatever state churn decided, and a wrong "this one needs you"
// trains the owner to ignore the badge, which destroys the feature.
func IsBlocked(agent, screen string) bool {
	d, ok := blockedRules[agent]
	if !ok {
		return false
	}
	return d.isBlocked(screen)
}

// MaxQuestion bounds a question's text, and each of its choices, in runes.
//
// Nothing else bounds them. `capture-pane -J` rejoins a question wrapped across
// rows, and the claude extractor joins a choice's continuation lines the same
// way, so one line of a wide pane is one long string -- and unlike a title,
// which tmux at least normalises, this rides every 1.5s poll into the sidebar
// and the tooltip. That is what MaxTitle exists for; this is the same hazard
// through the other field.
//
// Runes rather than bytes, and for the same reason as MaxLabel: the cap is a
// column budget for a row, and a byte cap would cut a readable question in half
// in any language that is not English. The number matches MaxTitle because it
// is the same row.
const MaxQuestion = 256

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
// Everything here fails closed: a dialog it cannot read yields nil rather than
// half a dialog. The caller keeps the state it already decided.
func ExtractQuestion(agent, screen string) *Question {
	d, ok := blockedRules[agent]
	if !ok {
		return nil
	}
	q := d.extractQuestion(screen)
	if q == nil {
		return nil
	}
	// Capped here rather than in each grammar, so that an agent added to the
	// table later cannot forget to do it: the rules table is meant to be edited
	// as data, and a length cap is not part of reading a dialog.
	q.Text = truncateRunes(q.Text, MaxQuestion)
	for i := range q.Choices {
		q.Choices[i] = truncateRunes(q.Choices[i], MaxQuestion)
	}
	return q
}

// truncateRunes cuts s to at most maxRunes runes.
//
// Ranging over the string rather than []rune(s): the index a range yields is a
// byte offset at a rune boundary, so this slices without allocating a rune
// slice the length of the screen, and cannot leave a half-rune on the wire the
// way s[:n] can.
func truncateRunes(s string, maxRunes int) string {
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i]
		}
		n++
	}
	return s
}

// --- Claude Code ------------------------------------------------------------

// claudeDialog matches Claude Code's permission dialog: a region delimited by
// horizontal rules, holding numbered choices under a line that asks.
//
// No single one of those is enough on its own, so all three are required.
type claudeDialog struct {
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

// isBlocked matches the bottommost rule-delimited region, because that is the
// live one. A dialog drawn above a fresh input box has already been answered. A
// screen with no rule on it at all has no dialog region and is never blocked.
//
// The footer -- "Esc to cancel · Tab to amend" -- is deliberately not required.
// It is the most style-volatile line on the screen and adds nothing over the
// cursor and the question, so requiring it would buy no strictness and cost
// every future restyle a missed badge.
func (d claudeDialog) isBlocked(screen string) bool {
	region, ok := dialogRegion(screen, d.rules)
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
		if d.cursorChoice.MatchString(line) {
			cursor = true
		}
		if d.choice.MatchString(line) {
			choices++
		}
		if d.question.MatchString(line) {
			asked = true
		}
	}
	return cursor && asked && choices >= d.minChoices
}

// extractQuestion reads the question and every option out of the same
// bottommost region isBlocked matched, so that the two can never disagree about
// which dialog on the screen they are looking at.
//
// Too few options, or no line asking anything above them, returns nil.
func (d claudeDialog) extractQuestion(screen string) *Question {
	region, ok := dialogRegion(screen, d.rules)
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
		if prefix := d.choice.FindString(body); prefix != "" {
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
	if first < 0 || len(choices) < d.minChoices {
		return nil
	}

	// The question is above the options, because that is how a dialog reads and
	// because the region can hold other lines ending in "?" -- a diff preview
	// of a file that contains one, say. The nearest such line above the first
	// choice is the one being asked; isBlocked does not care where it is,
	// because for a badge the existence of a question is the whole signal.
	text := ""
	for _, raw := range region[:first] {
		if body := strings.TrimSpace(raw); d.question.MatchString(body) {
			text = body
		}
	}
	if text == "" {
		return nil
	}
	return &Question{Text: text, Choices: choices}
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

// --- opencode ---------------------------------------------------------------

// opencodeDialog matches opencode's permission box, which says outright what
// Claude Code's only implies: its first line is "Permission required".
//
// So there is nothing here counting options, and nothing matching a question
// mark: opencode's box asks nothing, it announces. (The options are not
// countable anyway -- see extractQuestion.)
//
// What the header alone cannot say is which box on the screen it belongs to,
// and that is what the structure is for: it must be the first line of the
// bottommost guttered block. See isBlocked.
type opencodeDialog struct {
	// gutter is the rune opencode draws down the left edge of a block. Every
	// block is drawn this way -- the idle input box and each user message
	// included -- which is why the gutter delimits the region and never signals
	// anything by itself.
	gutter string
	// header is the block's title. The words are the claim; the "△" in front of
	// them is style, so any run of non-word characters is allowed before them
	// and none is required. Anchored at both ends because a title is the whole
	// line: the same words inside a sentence are prose.
	header *regexp.Regexp
	// request is the line naming what is being asked for -- "→ Edit foo.txt".
	// The arrow is matched literally and anchored, because unlike the header's
	// "△" it is not decoration next to the claim: it is the only thing that
	// says this line is the request rather than the file preview below it. If
	// opencode restyles it the quote goes and the badge stays, which is the
	// trade the whole extractor is built on.
	request *regexp.Regexp
}

// isBlocked matches a header at the top of the bottommost guttered block.
//
// Bottommost, because that is the live block. opencode's input box is itself
// guttered, so an answered permission box sits directly above one and must not
// keep the badge lit -- the same rule Claude Code's dialog gets from the
// bottommost rule, arrived at from the same reasoning.
//
// At the top, because everything below the header is content. The box carries a
// diff of the file it wants to write, and a file whose text happens to contain
// these words would otherwise light the badge from inside a preview. A title is
// the first thing in the block; blank rows above it are just the gutter.
func (d opencodeDialog) isBlocked(screen string) bool {
	return d.header.MatchString(firstContent(gutterBlock(screen, d.gutter)))
}

// extractQuestion returns the requested action -- "Edit fixture.txt" -- and no
// choices at all.
//
// Not because they are hard to find but because they cannot be told apart from
// the chrome beside them: opencode lays its options out sideways and puts the
// key hints on the same row, "Allow once   Allow always   Reject" and then
// "ctrl+f fullscreen  ⇆ select  enter confirm", separated by nothing but a
// wider run of spaces on the one capture anybody has. A column threshold read
// off a single screen is not a grammar, and matching the words themselves would
// read this permission type only and quote key hints as options on the next.
// Choices is omitempty for exactly this: the text is a convenience and half of
// it beats a wrong half.
//
// The first arrow line of the live block is the request. Like the Claude Code
// extractor, this does not re-check what isBlocked checked -- the caller has
// just run the authoritative copy -- so a screen it was never meant to see
// yields nil rather than a guess.
func (d opencodeDialog) extractQuestion(screen string) *Question {
	for _, line := range gutterBlock(screen, d.gutter) {
		if m := d.request.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return &Question{Text: m[1]}
		}
	}
	return nil
}

// gutterBlock returns the bottommost run of consecutive guttered lines with the
// gutter stripped. A screen with no gutter on it has no block, and gets nil --
// which no header matches and no request line is found in, so neither caller
// has a case for it.
//
// A line without the gutter ends the block, blank ones included: opencode draws
// the gutter down every row of a box, its empty rows too, so a gap really is
// the end of one block and not a hole in it.
func gutterBlock(screen, gutter string) []string {
	lines := strings.Split(screen, "\n")
	last := -1
	for i, line := range lines {
		if _, ok := guttered(line, gutter); ok {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	first := last
	for first > 0 {
		if _, ok := guttered(lines[first-1], gutter); !ok {
			break
		}
		first--
	}
	block := make([]string, 0, last-first+1)
	for _, line := range lines[first : last+1] {
		body, _ := guttered(line, gutter)
		block = append(block, body)
	}
	return block
}

// guttered returns what a line holds to the right of its gutter, and whether it
// had one. The gutter is indented from the left margin, so leading blanks are
// dropped before it is looked for.
func guttered(line, gutter string) (string, bool) {
	return strings.CutPrefix(strings.TrimLeft(line, " "), gutter)
}

// firstContent returns the first line of a block with anything on it, or "" for
// a block with nothing on it at all. A box's top rows are commonly the gutter
// and nothing else, and no header matches an empty line, so the empty block
// needs no case of its own.
func firstContent(block []string) string {
	for _, line := range block {
		if body := strings.TrimSpace(line); body != "" {
			return body
		}
	}
	return ""
}

// --- pi ----------------------------------------------------------------------

// piDialog matches pi's question overlay: a closed box holding a numbered list
// with one option highlighted.
//
// pi is different in kind from the other two. It has no permission dialog at
// all -- it asks through a tool, and the tool renders a two-pane selector: a
// filter, the options on the left, a description of the highlighted one on the
// right, key hints along the bottom. So there is nothing here matching a header
// and nothing requiring a question mark; what says a question is being asked is
// that a menu is open with something selected in it.
//
// Two consequences of that worth stating outright:
//
// A picker the operator opened themselves -- a model list, a theme list --
// renders through the same widget and lights the badge too. That is accepted
// rather than worked around: the pane really is waiting on a keystroke, and the
// only thing that would separate the two is the tool name pi writes into the
// box's top border. This capture says "ask_user", the extension that draws it
// is called "pi-ask-user" and the design document calls it "ask_question", so
// matching that name would miss every question raised by a tool named
// differently -- a badge missed for a question that was really asked is the
// expensive direction.
//
// Nothing here looks at the key hint line. It is the most style-volatile line
// in the overlay and it adds nothing over the cursor, so requiring it would buy
// no strictness and cost every restyle a missed badge -- and matching it
// loosely would fire on any pane whose output quotes pi's own help.
type piDialog struct {
	// top and bottom match the box's border lines. The corners are the whole
	// signal: pi is the only one of the three agents that draws a closed box,
	// and its overlay is the only closed box on its own screen. They are
	// regexps rather than runes because pi writes into both borders -- the tool
	// name into the top, its version into the bottom.
	top    *regexp.Regexp
	bottom *regexp.Regexp
	// wall is the rune down the box's sides, and also the rune splitting the
	// selector from the description beside it. It delimits a line's content; it
	// never signals anything by itself, because a table, a wrapped log line and
	// pi's own status bar all carry it.
	wall string
	// cursorChoice is the highlight sitting on a numbered option. This is the
	// load-bearing signal, exactly as it is for Claude Code: a list with nothing
	// highlighted is a list, and a list with something highlighted is a menu
	// waiting for a keystroke.
	cursorChoice *regexp.Regexp
	// choice is any numbered option, highlighted or not, and minChoices of them
	// are required because a question has alternatives. The unnumbered "Type
	// something" entry pi offers below the list is deliberately not one: it is
	// not numbered, so it is neither counted here nor quoted later.
	choice     *regexp.Regexp
	minChoices int
}

// isBlocked looks for the menu inside the bottommost fully drawn box.
//
// Bottommost because that is the live one, the same rule the other two agents
// get for the same reason. Fully drawn -- both borders -- because the box is
// the region: without the top there is nothing bounding it above, and the
// overlay is drawn complete, with its own scroll indicator when the prompt is
// taller than the box, so it does not run off the top of the screen.
func (d piDialog) isBlocked(screen string) bool {
	box, ok := d.box(screen)
	if !ok {
		return false
	}
	var choices int
	var cursor bool
	for _, line := range box {
		body := strings.TrimSpace(d.column(line))
		if body == "" {
			continue
		}
		if d.cursorChoice.MatchString(body) {
			cursor = true
		}
		if d.choice.MatchString(body) {
			choices++
		}
	}
	return cursor && choices >= d.minChoices
}

// box returns the lines between the bottommost bottom border and the nearest
// top border above it, and whether there was such a pair.
//
// Shared by detection and extraction so the two can never disagree about which
// box on the screen they are reading.
func (d piDialog) box(screen string) ([]string, bool) {
	lines := strings.Split(screen, "\n")
	bottom := -1
	for i, line := range lines {
		if d.bottom.MatchString(line) {
			bottom = i
		}
	}
	if bottom < 0 {
		return nil, false
	}
	for top := bottom - 1; top >= 0; top-- {
		if d.top.MatchString(lines[top]) {
			return lines[top+1 : bottom], true
		}
	}
	return nil, false
}

// column returns what a box line holds in its first column: the text between
// the left wall and whichever wall comes next.
//
// The overlay is drawn over the transcript rather than replacing it, so a line
// carries the text it covers on both sides of the box -- half a sentence to the
// left of the wall, half a word to the right. Reading the whole line would put
// that background text through the same rules as the box's own content, and the
// spinner still turning behind the overlay is in it.
//
// The first column is where both the prompt and the options are: a full-width
// line has one column, and where the selector splits the box in two, the
// options are on the left and the description of the highlighted one on the
// right. Nothing needs the description, so nothing reads it.
func (d piDialog) column(line string) string {
	open := strings.Index(line, d.wall)
	end := strings.LastIndex(line, d.wall)
	if open < 0 || end <= open {
		return ""
	}
	inner := line[open+len(d.wall) : end]
	if next := strings.Index(inner, d.wall); next >= 0 {
		inner = inner[:next]
	}
	return inner
}

// extractQuestion is deliberately absent for now: pi reports blocked, and the
// tooltip stays empty until a grammar for the overlay's text is proven.
func (d piDialog) extractQuestion(string) *Question { return nil }
