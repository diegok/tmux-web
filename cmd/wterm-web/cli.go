package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/diegok/tmux-web/internal/auth"
	"github.com/diegok/tmux-web/internal/front"
)

// The CLI is the half of the auth chain that runs as a person. Everything but
// `serve` is a client of the admin socket, and the socket is the whole
// authorization argument: a remote browser carries no OS identity, a local unix
// socket does, and SO_PEERCRED turns "can you run this command as me on this
// box" into the proof that mints a credential. So these subcommands take no
// password, no key, and no --user: being able to open the socket *is* the
// credential.
//
// Stream discipline, uniform across subcommands: stdout carries the answer and
// nothing else, stderr carries commentary. `enroll` therefore prints the bare
// URL on stdout and its "single use, expires in 10m" note on stderr, so
// `wterm-web enroll --name phone | qrencode -t ansiutf8` works without any
// --quiet flag.

// adminRequestTimeout bounds one admin call. The daemon fsyncs the device file
// on enroll and revoke, so this is generous; the point is that a wedged daemon
// makes the CLI fail rather than hang forever with no output.
const adminRequestTimeout = 15 * time.Second

const usageText = `wterm-web -- drive the local tmux server from a browser.

Usage:
  wterm-web serve   --host <hostname> [--dev | --self-signed | --tls-cert F --tls-key F]
  wterm-web enroll  --name <device>
  wterm-web devices
  wterm-web revoke  <device-id>

serve runs the daemon. The other three talk to its admin socket, which only
the user the daemon runs as can open -- that is the whole authorization.

Run "wterm-web <command> -h" for a command's flags.
`

func printUsage(w io.Writer) { fmt.Fprint(w, usageText) }

// run is the CLI, with its arguments and streams as parameters so a test can
// drive it in-process against an admin socket of its own. It returns the
// process exit code: 0 success, 1 a failed operation, 2 a malformed command
// line.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(rest, stdout, stderr)
	case "enroll":
		return cmdEnroll(rest, stdout, stderr)
	case "devices":
		return cmdDevices(rest, stdout, stderr)
	case "revoke":
		return cmdRevoke(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "wterm-web: unknown command %q\n\n", cmd)
		printUsage(stderr)
		return 2
	}
}

// -- subcommands ------------------------------------------------------------

// serveConfig is what `serve` parses out of its command line. It stays a
// separate type from front.Config: the command line is a user interface with
// its own defaults and its own error messages, and the daemon takes several
// things -- a state path, tmux arguments, a poll interval -- that no flag
// exposes.
type serveConfig struct {
	Host       string
	Dev        bool
	Port       int
	Socket     string
	TLSCert    string
	TLSKey     string
	SelfSigned bool
	TLSPort    int
}

// serveRun starts the daemon. A variable rather than a direct call so a test
// can drive the command line without binding a port or asking a CA for a
// certificate.
var serveRun = runDaemon

// runDaemon is `serve`: logging to stderr, a signal handler, and the front
// server.
//
// Ctrl-C and SIGTERM stop it. There is nothing to flush on the way out -- tmux
// holds the terminal state and the device file is fsynced on every write -- so
// the shutdown exists to close listeners and let systemd see a clean exit
// rather than to save anything.
func runDaemon(cfg serveConfig, stdout, stderr io.Writer) error {
	// The daemon is the only subcommand that logs: the others print an answer
	// and exit. Wiring the default logger here rather than in main keeps a
	// stray slog call in a CLI path from writing over that answer.
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return front.Serve(ctx, front.Config{
		Host:        cfg.Host,
		Dev:         cfg.Dev,
		Port:        cfg.Port,
		AdminSocket: cfg.Socket,
		TLSCert:     cfg.TLSCert,
		TLSKey:      cfg.TLSKey,
		SelfSigned:  cfg.SelfSigned,
		TLSPort:     cfg.TLSPort,
	})
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	fset := newFlagSet("serve", stderr,
		"wterm-web serve --host <hostname> [--dev | --self-signed | --tls-cert F --tls-key F]")
	host := fset.String("host", "", "public hostname browsers reach this daemon on, e.g. tmux.example.com")
	dev := fset.Bool("dev", false, "serve plain HTTP on loopback instead of getting a certificate")
	port := fset.Int("port", 8080, "listen port in --dev mode")
	selfSigned := fset.Bool("self-signed", false,
		"generate and reuse a certificate for --host, for a name no public CA can validate")
	tlsCert := fset.String("tls-cert", "", "PEM certificate to serve, e.g. from mkcert or an internal CA")
	tlsKey := fset.String("tls-key", "", "PEM private key for --tls-cert")
	tlsPort := fset.Int("tls-port", 443, "HTTPS port for --self-signed or --tls-cert")
	socket := socketFlag(fset)
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	if len(operands) > 0 {
		return usageError(stderr, fset, "serve takes no positional arguments, got %q", operands[0])
	}
	// --host is required even with --dev: it is what the origin allowlist and
	// the certificate are derived from, and a daemon that guessed its own
	// public name would guess wrong exactly once.
	if strings.TrimSpace(*host) == "" {
		return usageError(stderr, fset, "--host is required: the hostname browsers will use, e.g. tmux.example.com")
	}

	// Each mode decides how a browser gets a trusted connection, so asking for
	// two is a contradiction rather than a preference to resolve silently.
	modes := 0
	for _, on := range []bool{*dev, *selfSigned, *tlsCert != "" || *tlsKey != ""} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return usageError(stderr, fset, "choose one of --dev, --self-signed or --tls-cert/--tls-key")
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return usageError(stderr, fset, "--tls-cert and --tls-key must be given together")
	}

	cfg := serveConfig{
		Host:       strings.TrimSpace(*host),
		Dev:        *dev,
		Port:       *port,
		Socket:     *socket,
		TLSCert:    *tlsCert,
		TLSKey:     *tlsKey,
		SelfSigned: *selfSigned,
		TLSPort:    *tlsPort,
	}
	if err := serveRun(cfg, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	fset := newFlagSet("enroll", stderr, "wterm-web enroll --name <device>")
	name := fset.String("name", "", "what to call the device in the devices list, e.g. laptop")
	socket := socketFlag(fset)
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	if len(operands) > 0 {
		return usageError(stderr, fset, "enroll takes no positional arguments, got %q (did you mean --name %s?)", operands[0], operands[0])
	}
	// Checked here as well as in the daemon so the message names the flag, and
	// so an empty name costs no round trip.
	if strings.TrimSpace(*name) == "" {
		return usageError(stderr, fset, "--name is required: something you will recognise in the devices list")
	}

	client, err := newAdminClient(*socket)
	if err != nil {
		return fail(stderr, err)
	}
	var resp struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := client.do(http.MethodPost, "/enroll", map[string]string{"name": strings.TrimSpace(*name)}, &resp); err != nil {
		return fail(stderr, err)
	}
	if err := checkEnrollURL(resp.URL); err != nil {
		return fail(stderr, err)
	}

	fmt.Fprintln(stdout, resp.URL)
	fmt.Fprintf(stderr, "single use, expires in %s -- open it on %s\n",
		shortDuration(auth.EnrollTTL), display(resp.Name, 40))
	return 0
}

func cmdDevices(args []string, stdout, stderr io.Writer) int {
	fset := newFlagSet("devices", stderr, "wterm-web devices")
	socket := socketFlag(fset)
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	if len(operands) > 0 {
		return usageError(stderr, fset, "devices takes no positional arguments, got %q (to remove one: wterm-web revoke %s)", operands[0], operands[0])
	}

	client, err := newAdminClient(*socket)
	if err != nil {
		return fail(stderr, err)
	}
	var resp struct {
		Devices []deviceRow `json:"devices"`
	}
	if err := client.do(http.MethodGet, "/devices", nil, &resp); err != nil {
		return fail(stderr, err)
	}
	if len(resp.Devices) == 0 {
		// On stderr, and stdout stays empty: a lone header row reads as a
		// broken command, and a note on stdout would land in whatever the
		// output was piped into.
		fmt.Fprintln(stderr, "no devices enrolled; run: wterm-web enroll --name <device>")
		return 0
	}
	writeDeviceTable(stdout, resp.Devices, time.Now())
	return 0
}

func cmdRevoke(args []string, stdout, stderr io.Writer) int {
	fset := newFlagSet("revoke", stderr, "wterm-web revoke <device-id>")
	socket := socketFlag(fset)
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	switch len(operands) {
	case 1:
	case 0:
		return usageError(stderr, fset, "revoke needs a device id; run: wterm-web devices")
	default:
		// Not "revoke the first and ignore the rest": someone pasting two ids
		// would be told one device was cut off while the other kept its shell.
		return usageError(stderr, fset, "revoke takes exactly one device id, got %d", len(operands))
	}
	id := operands[0]

	client, err := newAdminClient(*socket)
	if err != nil {
		return fail(stderr, err)
	}
	if err := client.do(http.MethodDelete, "/devices/"+url.PathEscape(id), nil, nil); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "revoked %s\n", display(id, 64))
	return 0
}

// -- the admin client -------------------------------------------------------

type adminClient struct {
	socket string
	http   *http.Client
}

// newAdminClient dials the admin socket, defaulting to wherever the daemon
// would have put it.
func newAdminClient(socket string) (*adminClient, error) {
	if socket == "" {
		p, err := auth.AdminSocketPath()
		if err != nil {
			return nil, fmt.Errorf("cannot work out where the admin socket lives: %w", err)
		}
		socket = p
	}
	hc := auth.AdminClient(socket)
	// Redirects are refused rather than followed. The admin API never issues
	// one, so a 3xx means a request landed somewhere unintended -- and Go's
	// client turns a followed 301 into a GET, which would quietly convert a
	// DELETE that missed its route into a successful read. Surfacing the 3xx
	// makes that a visible failure instead of a revoke that reports success
	// without revoking anything.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &adminClient{socket: socket, http: hc}, nil
}

// do performs one admin request. body is JSON-encoded when non-nil, and the
// response is decoded into out when non-nil.
func (c *adminClient) do(method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s request: %w", method, path, err)
		}
		payload = bytes.NewReader(buf)
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, auth.AdminBaseURL+path, payload)
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return c.describeFailure(err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode >= 300 {
		return c.statusError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("the daemon's answer to %s %s was not the JSON we expected: %w", method, path, err)
	}
	return nil
}

// statusError turns the admin API's {"error": ...} body into an error. The
// caller is the owner on a socket only they can open, so the daemon's own words
// are the most useful thing to print.
func (c *adminClient) statusError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	if msg := strings.TrimSpace(body.Error); msg != "" {
		return errors.New(msg)
	}
	return fmt.Errorf("the daemon refused the request: %s", resp.Status)
}

// describeFailure turns a dial failure into an answer.
//
// "dial unix /run/user/1000/wterm-web.sock: connect: no such file or directory"
// is the single most likely thing this CLI ever prints, and on its own it tells
// the owner nothing about what to do. Each case below has a different fix, so
// each gets a different sentence.
func (c *adminClient) describeFailure(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("the daemon did not answer within %s (socket %s); it may be wedged", shortDuration(adminRequestTimeout), c.socket)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("wterm-web does not appear to be running: there is no admin socket at %s.\n"+
			"  start it with:  wterm-web serve --host <hostname>\n"+
			"  already running elsewhere? point at its socket with --socket <path>", c.socket)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("wterm-web is not running: %s is left over from a daemon that exited.\n"+
			"  start it with:  wterm-web serve --host <hostname>", c.socket)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("cannot open the admin socket %s: permission denied.\n"+
			"  it belongs to the user the daemon runs as -- run this command as that user", c.socket)
	}
	return fmt.Errorf("cannot reach the daemon on %s: %w", c.socket, err)
}

// checkEnrollURL refuses a link that would leak its own token.
//
// The daemon builds this URL and puts the token after '#' precisely so it is
// never sent to a server: it stays out of access logs and Referer headers, and
// the link scanners messaging apps run do not redeem it in passing. The CLI is
// the last place a human sees the link before pasting it somewhere, and a
// version skew or a mangled BaseURL is the way the token could arrive in the
// query instead. Checking here costs one parse and turns a silent leak into a
// loud failure, so it is not redundant with the daemon's own care.
func checkEnrollURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("the daemon returned an enrollment link that is not a URL (%q): %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("the daemon returned an enrollment link with no usable scheme: %q", raw)
	}
	// The query is checked first: a link with a token in it is an active leak,
	// where a link with no fragment is merely broken.
	if u.RawQuery != "" {
		return fmt.Errorf("refusing to print an enrollment link carrying a query string (%q): a token in the query lands in access logs and Referer headers", raw)
	}
	if u.Fragment == "" {
		return fmt.Errorf("refusing to print an enrollment link with an empty fragment (%q): the token belongs after '#', where it is never sent to a server", raw)
	}
	return nil
}

// -- devices table ----------------------------------------------------------

// deviceRow is one row of GET /devices. It is declared here rather than shared
// with the daemon so the wire format is what couples them: adding a field to
// the store never leaks it into this table by accident.
type deviceRow struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// userAgentWidth caps the browser column. It is last so that a terminal too
// narrow for it wraps only decoration.
const userAgentWidth = 44

// writeDeviceTable renders the devices a person reads before deciding which one
// to revoke.
//
// The columns answer the two questions that decision needs: which of my devices
// is this (NAME, BROWSER), and is it still in use (LAST SEEN). ID is first
// because it is the argument `revoke` takes, so it can be double-clicked out of
// the leftmost column. Times are relative -- "3d ago" -- because recognition is
// comparative and nobody subtracts RFC 3339 timestamps in their head; past a
// month a relative age stops meaning anything ("417d ago") and the date is
// printed instead.
func writeDeviceTable(w io.Writer, devices []deviceRow, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tLAST SEEN\tENROLLED\tBROWSER")
	for _, d := range devices {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			display(d.ID, 64),
			display(d.Name, 24),
			humanAge(d.LastSeen, now),
			humanAge(d.CreatedAt, now),
			display(d.UserAgent, userAgentWidth),
		)
	}
	tw.Flush()
}

// humanAge renders when something last happened, relative to now.
func humanAge(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < -time.Minute:
		// Clock skew, or a file edited by hand. Saying "0s ago" would hide it.
		return t.Local().Format("2006-01-02") + " (future)"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// display makes a string from the daemon safe to print on a terminal and short
// enough for a column.
//
// Sanitizing is not cosmetic. The user-agent comes from whichever browser
// redeemed an enrollment link, and a leaked link redeemed by someone else puts
// a string of their choosing into this table. A user-agent containing ANSI
// escapes could clear the screen, move the cursor, or hide the row above it --
// which is to say, hide the device the owner was looking for. Every
// non-printable rune therefore becomes '?', visibly.
func display(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		if n == max {
			// A rune, not "...", so the truncation cannot be mistaken for part
			// of the value.
			b.WriteRune('…')
			break
		}
		if r == utf8RuneError || !unicode.IsPrint(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	if n == 0 {
		return "-"
	}
	return b.String()
}

// utf8RuneError is U+FFFD, what invalid UTF-8 decodes to. Ranging over a string
// yields it for every bad byte, and it is printable, so it needs naming here to
// be caught by the same rule as a control character.
const utf8RuneError = '�'

// shortDuration renders a whole-minute duration the way a person would say it.
func shortDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}

// -- flag plumbing ----------------------------------------------------------

// newFlagSet returns a subcommand's flags. The stdlib has no subcommands, and
// one FlagSet per verb is the whole of what this CLI needs from a framework:
// four commands, six flags, no shared global state to get out of order.
func newFlagSet(name string, stderr io.Writer, synopsis string) *flag.FlagSet {
	fset := flag.NewFlagSet(name, flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s\n\n", synopsis)
		fset.PrintDefaults()
	}
	return fset
}

// socketFlag adds --socket to a subcommand.
//
// Task 13 left this open, having no caller yet. It earns its place: the daemon
// and the CLI each compute the socket path from their own environment, and
// those environments diverge in the ordinary case -- a daemon started by
// systemd --user has $XDG_RUNTIME_DIR, an ssh session without a login manager
// does not, and the two then disagree about where the socket is. Without an
// override, the CLI's only report is "not running" about a daemon that is
// plainly running, and there is nothing the owner can do about it from the
// command line. It is on `serve` too, because an escape hatch that moves only
// one end of a socket does not connect anything.
func socketFlag(fset *flag.FlagSet) *string {
	return fset.String("socket", "", "admin socket path (default: $XDG_RUNTIME_DIR/wterm-web.sock)")
}

// parseFlags parses args, returning the positional arguments and, when the
// caller should stop, the exit code to stop with. An explicit -h is a
// successful request for help, not a mistake.
func parseFlags(fset *flag.FlagSet, args []string) ([]string, int, bool) {
	operands, err := parseInterspersed(fset, args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return nil, 0, false
	case err != nil:
		// flag has already printed the error and the usage.
		return nil, 2, false
	}
	return operands, 0, true
}

// parseInterspersed parses args, allowing flags to follow positional
// arguments.
//
// The stdlib stops at the first non-flag word, which would make
// "revoke <id> --socket /path" put "--socket" and "/path" in the positional
// list and then reject the command for having three ids. That is the order a
// person types -- the id is the subject, the flag an afterthought -- and a CLI
// that refuses it is one people learn to distrust. Re-parsing after each
// operand costs nothing at this size and needs no knowledge of which flags
// take a value.
//
// A "--" terminator needs no handling of its own: flag.Parse already stops
// there and hands back the rest, and since no subcommand here takes more than
// one operand, the loop cannot get far enough to re-parse one of them as a
// flag. A subcommand that took several would need the terminator honoured
// explicitly.
func parseInterspersed(fset *flag.FlagSet, args []string) ([]string, error) {
	var operands []string
	for {
		if err := fset.Parse(args); err != nil {
			return nil, err
		}
		rest := fset.Args()
		if len(rest) == 0 {
			return operands, nil
		}
		operands = append(operands, rest[0])
		args = rest[1:]
	}
}

// usageError reports a malformed command line: exit 2, distinct from the 1 a
// failed operation returns, so a script can tell "I typed it wrong" from "the
// device is still enrolled".
func usageError(stderr io.Writer, fset *flag.FlagSet, format string, args ...any) int {
	fmt.Fprintf(stderr, "wterm-web %s: %s\n\n", fset.Name(), fmt.Sprintf(format, args...))
	fset.Usage()
	return 2
}

// fail reports a failed operation: exit 1.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "wterm-web: %v\n", err)
	return 1
}
