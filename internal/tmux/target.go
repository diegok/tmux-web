package tmux

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Validation of everything the browser is allowed to put on a tmux command
// line. Management widens v1's blast radius a long way -- the browser can now
// ask to kill things -- and tmux is unhelpful about bad targets in two
// specific ways:
//
//   - An empty target is not an error. It means "whatever is current", so an
//     id the frontend failed to fill in kills or renames an arbitrary object
//     and exits 0.
//   - Name matching falls back to a prefix without reporting the ambiguity.
//     v1 measured this: `kill-session -t _web-` killed _web-abcd and exited 0.
//
// Ids are therefore preferred everywhere over names -- a stale id targets
// nothing, while a stale name may target something else -- and the one name
// that must still come from the browser, the one for a new session, is held to
// what tmux can address afterwards.

// MaxSessionName bounds a session name in runes.
//
// tmux imposes no limit of its own: a 3000-character name was accepted and
// stored (probed on tmux 3.7b). Names are typed by the owner into a dialog and
// ride every poll into a sidebar row, so the cap is about what stays readable
// there, not about what tmux can hold. Runes rather than bytes for the same
// reason: "ñ" takes one column and two bytes, and the cap is a column budget.
const MaxSessionName = 128

// ValidatePaneID reports whether s is a tmux pane id such as "%3".
func ValidatePaneID(s string) error { return validateID("pane", '%', s) }

// ValidateWindowID reports whether s is a tmux window id such as "@7".
func ValidateWindowID(s string) error { return validateID("window", '@', s) }

// ValidateSessionID reports whether s is a tmux session id such as "$1".
func ValidateSessionID(s string) error { return validateID("session", '$', s) }

// validateID accepts a sigil followed by one or more ASCII digits, and nothing
// else.
//
// The sigil is checked per kind rather than "is it one of %@$", because tmux
// resolves the wrong kind rather than refusing it: `kill-window -t %3` kills
// the window *containing* pane 3. Accepting a pane id where a window id
// belongs would destroy more than the browser asked for, successfully.
//
// Digits are compared as bytes, so a rune that unicode.IsDigit calls a digit --
// Arabic-Indic "١", full-width "１" -- is rejected. tmux parses ids with
// strtonum, which knows only ASCII.
func validateID(kind string, sigil byte, s string) error {
	if len(s) < 2 || s[0] != sigil {
		return fmt.Errorf("%q is not a tmux %s id (want %c followed by digits)", s, kind, sigil)
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return fmt.Errorf("%q is not a tmux %s id (want %c followed by digits)", s, kind, sigil)
		}
	}
	return nil
}

// ValidateSessionName reports whether name is safe to create a session with.
//
// Every rejection here is a thing tmux permits and then cannot undo. All of it
// was probed against tmux 3.7b on a throwaway socket:
//
//   - Empty. `new-session -s ""` exits 0 and creates a session with no name,
//     and `kill-session -t "="` answers "no mouse target". The session is
//     unkillable by name and invisible in the sidebar.
//   - Leading "-". A new name is a positional argument to rename-session, so
//     tmux reads it as a flag: "command rename-session: unknown flag -x".
//   - ":" or ".". These are tmux's target separators, and they are split off
//     before the name is matched -- even with the "=" exact-match prefix.
//     `kill-session -t "=a:b"` answers "can't find session: a", and
//     `kill-session -t "=a.b"` answers "can't find window: a". A session so
//     named can never be addressed by name again.
//
// Control characters are refused too. tmux refuses most of them itself, but its
// check is byte-oriented -- below 0x20, plus 0x7f -- so a C1 control such as
// U+009F is accepted and stored, and would ride the poll into the DOM. A 0x1f
// in particular is the snapshot field separator, which forges a record and
// makes a pane vanish from the sidebar; that is the same hole @tmux_web_label is
// validated against on write (validateLabel) and repaired against on read
// (snapshot.go's sanitizeLabel), the two sharing one notion of a safe value.
//
// Deliberately allowed: inner, leading and trailing spaces (arguments reach
// tmux through exec, never a shell, and such a name is still addressable);
// non-ASCII (tmux stores it fine); and the "_web-" prefix, since app sessions
// are identified by the @tmux_web_owned option and never by name -- a user may
// legitimately want that name, and taking it from them is the bug session.go
// already warns about.
func ValidateSessionName(name string) error { return validateName("session", name) }

// ValidateWindowName reports whether name is safe to give a window.
//
// A sibling rather than a reuse of ValidateSessionName, because the error text
// reaches the owner in a toast and "invalid session name" on a window rename is
// a lie about what went wrong. The rules themselves are shared: every hazard in
// ValidateSessionName's list was re-probed against tmux 3.7b for windows and
// found identical, and one of them is worse.
//
//   - Empty. `rename-window -t @1 ""` exits 0 and stores the empty string, so
//     unlike a session -- which at least keeps its old name until something
//     addresses it -- a window ends up with a blank sidebar row immediately.
//     (`new-window -n ""` is quieter: automatic-rename fills a name in.)
//   - ":" and "." are the same target separators, and a window is addressed as
//     session:window, so `a:b` and `w.y` are both unaddressable by name.
//   - A leading "-" is read as a flag in rename-window's positional slot.
//   - C1 controls, 0x1f included, are stored verbatim: tmux rejected
//     "A\x1fB" but accepted the same name with U+009F in place of the 0x1f.
//
// The rune cap is MaxSessionName for the same column-budget reason; a window
// name occupies the same kind of sidebar row.
func ValidateWindowName(name string) error { return validateName("window", name) }

func validateName(kind, name string) error {
	// Before the rune count, because counting an invalid string is meaningless.
	// encoding/json rewrites invalid UTF-8 to U+FFFD without erroring, so a
	// name that survived this would be displayed as something tmux does not
	// hold, and the owner would be clicking a lie.
	if !utf8.ValidString(name) {
		return fmt.Errorf("invalid %s name %q: not valid UTF-8", kind, name)
	}
	// TrimSpace, not len: a name of spaces is a row the owner can neither read
	// nor tell apart from the next one.
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("invalid %s name %q: empty", kind, name)
	}
	if n := utf8.RuneCountInString(name); n > MaxSessionName {
		return fmt.Errorf("invalid %s name: %d characters, limit is %d", kind, n, MaxSessionName)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid %s name %q: cannot start with '-', tmux reads it as a flag", kind, name)
	}
	if i := strings.IndexAny(name, ":."); i >= 0 {
		return fmt.Errorf("invalid %s name %q: cannot contain %q, tmux uses it to separate targets", kind, name, name[i])
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid %s name %q: contains a control character (%U)", kind, name, r)
		}
	}
	return nil
}
