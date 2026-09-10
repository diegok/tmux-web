package tmux

import (
	"strings"
	"testing"
)

// idCase is one input to an id validator. why says what the rejection prevents;
// it is printed on failure, so a regression reports the reason the case exists
// rather than just the input that broke.
type idCase struct {
	in   string
	want bool // want valid
	why  string
}

// idKinds pairs each validator with the sigil tmux prints for that object, so
// the shared table below is instantiated once per kind. A validator checking
// the wrong sigil fails its own accept cases; one checking no sigil at all
// accepts another kind's ids, which the cross-kind cases below catch.
var idKinds = []struct {
	fname string
	name  string
	sigil string
	fn    func(string) error
}{
	{"ValidatePaneID", "pane", "%", ValidatePaneID},
	{"ValidateWindowID", "window", "@", ValidateWindowID},
	{"ValidateSessionID", "session", "$", ValidateSessionID},
}

func TestValidateID(t *testing.T) {
	for _, k := range idKinds {
		s := k.sigil
		cases := []idCase{
			{s + "0", true, ""},
			{s + "3", true, ""},
			{s + "12", true, ""},
			// tmux never prints a padded id, but %007 and %7 name the same
			// object to it, so refusing the padded form would buy nothing.
			{s + "007", true, ""},

			{"", false, "tmux resolves an empty target to 'whatever is current' and exits 0, so an id the frontend failed to fill in would kill or rename an arbitrary object"},
			{s, false, "a bare sigil is not an id: tmux reads it as a target name and falls back to prefix matching"},
			{s + "abc", false, "a non-numeric suffix is not an id"},
			{s + "1a", false, "a trailing non-digit is not an id"},
			{s + "-1", false, "there are no negative ids, and a '-' that reaches a tmux argument is read by getopt as a flag"},
			{s + "+1", false, "a sign is not part of an id; tmux prints digits only"},
			{s + " 1", false, "whitespace after the sigil rides through to the target"},
			{s + "1 ", false, "a trailing space makes a different target string than the id it looks like"},
			{s + "1\n", false, "a trailing newline would forge a second line wherever the id is echoed into a -F listing"},
			{"1", false, "a bare number is a window or session *index*, not an id, and indexes are relative to whatever is current"},
			{" " + s + "1", false, "tmux does not trim a leading space off a target"},
			{s + "1" + s + "2", false, "two ids in one argument name neither"},
			{s + "١", false, "an Arabic-Indic digit satisfies unicode.IsDigit but is not a digit to tmux"},
			{s + "１", false, "a full-width digit satisfies unicode.IsDigit but is not a digit to tmux"},
			{"work:1", false, "a name:index target is not an id; ids are preferred precisely because a stale id targets nothing, while a stale name may target something else"},
		}
		// Every other kind's valid id must be rejected. tmux resolves a pane id
		// given where a window id belongs -- `kill-window -t %3` kills the
		// window *containing* pane 3 -- so a sigil mix-up destroys more than
		// was asked for, and exits 0 doing it.
		for _, other := range idKinds {
			if other.sigil == s {
				continue
			}
			cases = append(cases, idCase{other.sigil + "1", false,
				"a " + other.name + " id is not a " + k.name + " id"})
		}

		for _, tc := range cases {
			err := k.fn(tc.in)
			switch {
			case tc.want && err != nil:
				t.Errorf("%s(%q) = %v, want nil", k.fname, tc.in, err)
			case !tc.want && err == nil:
				t.Errorf("%s(%q) = nil, want an error: %s", k.fname, tc.in, tc.why)
			}
		}
	}
}

// The error names the offending value, because it is what the endpoint reports
// to the browser and "invalid id" gives the owner nothing to act on.
func TestValidateIDErrorNamesTheInput(t *testing.T) {
	err := ValidatePaneID("@1")
	if err == nil {
		t.Fatal("ValidatePaneID(@1) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "@1") {
		t.Errorf("ValidatePaneID(@1) = %q, want the message to name the input", err)
	}
}

func TestValidateSessionName(t *testing.T) {
	long := strings.Repeat("z", MaxSessionName)
	for _, tc := range []struct {
		in   string
		want bool
		why  string
	}{
		{"work", true, ""},
		{"my project", true, "an inner space is harmless: arguments go to exec, never through a shell"},
		{"a-b", true, "a dash that is not leading is an ordinary character"},
		{"señor", true, "tmux stores non-ASCII names fine, and refusing them would be parochial"},
		{"_web-notes", true, "app sessions are identified by the @wterm_web option and never by name, so this prefix stays the user's to take"},
		{" lead", true, "a leading or trailing space is addressable and harmless; only an all-whitespace name is refused"},
		{long, true, "exactly at the cap is allowed"},

		{"", false, "tmux creates a session with an empty name and exits 0; `kill-session -t '='` then answers 'no mouse target', so that session can never be killed by name (probed)"},
		{"   ", false, "a whitespace-only name is a sidebar row the owner cannot see, read or tell apart from another one"},
		{"-x", false, "a name reaches `rename-session <new-name>` as a positional argument, where tmux reads it as a flag: 'command rename-session: unknown flag -x' (probed)"},
		{"-", false, "a bare dash is the same flag hazard"},
		{"a:b", false, "':' separates session from window in a target, so `kill-session -t '=a:b'` answers \"can't find session: a\" -- the session cannot be addressed by name at all (probed)"},
		{":b", false, "the same separator, leading"},
		{"a:", false, "the same separator, trailing"},
		{"a.b", false, "'.' separates window from pane, so `kill-session -t '=a.b'` answers \"can't find window: a\" (probed)"},
		{".", false, "a bare dot is the same separator hazard"},
		{"a\nb", false, "a newline splits one snapshot record into two and makes a pane vanish from the sidebar; tmux refuses it as well, but the browser should get our error rather than tmux's"},
		{"a\x1fb", false, "0x1f is the snapshot field separator: it forges a record"},
		{"a\tb", false, "a tab is a control byte, which tmux itself refuses in a session name"},
		{"a\x7fb", false, "DEL is a control byte"},
		{"a\u009fb", false, "a C1 control byte: tmux's own check looks at bytes below 0x20 plus 0x7f and ACCEPTS this one (probed, exit 0), so nothing else stops it riding the poll into the DOM"},
		{long + "z", false, "one rune over the cap; tmux imposes no limit of its own -- a 3000-character name was accepted and stored (probed)"},
		{"a\xffb", false, "invalid UTF-8 is rewritten to U+FFFD by encoding/json, so the sidebar would show a name tmux does not hold"},
	} {
		err := ValidateSessionName(tc.in)
		switch {
		case tc.want && err != nil:
			t.Errorf("ValidateSessionName(%q) = %v, want nil: %s", tc.in, err, tc.why)
		case !tc.want && err == nil:
			t.Errorf("ValidateSessionName(%q) = nil, want an error: %s", tc.in, tc.why)
		}
	}
}

// The cap counts runes, not bytes: a name of ASCII letters and one of two-byte
// runes take the same room in the sidebar, which is what the cap is for.
func TestValidateSessionNameCapsRunesNotBytes(t *testing.T) {
	name := strings.Repeat("ñ", MaxSessionName)
	if err := ValidateSessionName(name); err != nil {
		t.Errorf("ValidateSessionName(%d two-byte runes) = %v, want nil", MaxSessionName, err)
	}
}

// ValidateWindowName is a sibling of ValidateSessionName rather than a call to
// it, and the only difference is the one that matters to the owner: the message
// names the thing they were renaming. A shared implementation is what makes the
// rules identical; this pins that neither half drifted.
func TestValidateWindowNameSharesTheRulesAndNamesItsOwnKind(t *testing.T) {
	// U+009F is in the list because tmux's own control-character check is
	// byte-oriented and lets C1 through, for windows exactly as for sessions.
	for _, name := range []string{"", "   ", "a:b", "w.y", "-z", "a\x1fb", "a\u009fb",
		strings.Repeat("z", MaxSessionName+1)} {
		werr, serr := ValidateWindowName(name), ValidateSessionName(name)
		if werr == nil {
			t.Errorf("ValidateWindowName(%q) = nil, want an error: tmux stores it and the row breaks", name)
			continue
		}
		if serr == nil {
			t.Errorf("ValidateSessionName(%q) = nil while the window rule rejects it: the two have drifted", name)
		}
		if !strings.Contains(werr.Error(), "window name") {
			t.Errorf("ValidateWindowName(%q) says %q; a toast on a window rename must not say \"session\"", name, werr)
		}
	}
	if err := ValidateWindowName("api build"); err != nil {
		t.Errorf("ValidateWindowName(%q) = %v, want nil", "api build", err)
	}
}
