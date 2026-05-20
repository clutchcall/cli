package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/clutchcall/cli/internal/auth"
	"github.com/clutchcall/cli/internal/calls"
	"github.com/clutchcall/cli/internal/docs"
	"github.com/clutchcall/cli/internal/migrate"
	"github.com/clutchcall/cli/internal/scaffold"
	"github.com/clutchcall/cli/internal/trunks"
)

const version = "0.1.0"

// brand is the user-facing binary name baked in at link time. The
// Makefile passes -ldflags '-X main.brand=tq' to produce a TeleQuick
// build; the default `clutch` matches the source-of-truth ClutchCall
// repo. brandify() rewrites every "clutch " token in help text so
// every "Usage:" line and example command shows the correct name.
var brand = "clutch"

func brandify(s string) string {
	if brand == "clutch" {
		return s
	}
	return strings.ReplaceAll(s, "clutch ", brand+" ")
}

var usage = brandify(`clutch — ClutchCall SDK developer CLI

Usage:
  clutch <command> [args]

Commands:
  auth <subcommand>    Authenticate against the portal (login|logout|whoami)
  trunks <subcommand>  Inspect SIP trunks (list|show)
  dial <e164>          Originate a call through a trunk
  hangup <call_sid>    Terminate an active call
  transfer <sid> <dst> Blind transfer (RFC 3515 SIP REFER)
  mute <sid>           Mute the active call (--wire to also recvonly re-INVITE)
  unmute <sid>         Unmute (--wire to also re-INVITE sendrecv)
  hold <sid>           Place call on hold (sendrecv → sendonly re-INVITE)
  unhold <sid>         Resume held call
  send-dtmf <sid> <d>  Send a DTMF digit (--mode rfc2833|info)
  init <lang> <name>   Scaffold a new ClutchCall project (lang: go|typescript|python)
  migrate              Diff a project's pinned schema against the current SDK and report breaking changes
  docs <subcommand>    Browse ClutchCall documentation (overview|search|get-page)
  version              Print CLI version

Environment:
  CLUTCH_BASE_URL      Portal/BFF base URL (default: https://portal.clutchcall.dev)
  CLUTCH_DOCS_BASE     Docs base URL for the docs subcommand (default: https://docs.clutchcall.dev)
  CLUTCH_TOKEN         Override saved access token (useful in CI)
  CLUTCH_CONFIG_DIR    Override credentials dir (default: per-OS config dir, see ` + "`clutch auth whoami`" + `)
  CLUTCH_DEBUG=1       Dump tRPC requests + raw responses to stderr

Examples:
  clutch auth login
  clutch init go my-agent
  clutch docs search "originate trunk"
`)

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "auth":
		runAuth(os.Args[2:])
	case "trunks":
		runTrunks(os.Args[2:])
	case "dial":
		runDial(os.Args[2:])
	case "hangup":
		runHangup(os.Args[2:])
	case "transfer":
		runTransfer(os.Args[2:])
	case "mute":
		runMuteUnmute(os.Args[2:], true)
	case "unmute":
		runMuteUnmute(os.Args[2:], false)
	case "hold":
		runHoldUnhold(os.Args[2:], true)
	case "unhold":
		runHoldUnhold(os.Args[2:], false)
	case "send-dtmf":
		runSendDTMF(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
	case "migrate":
		runMigrate(os.Args[2:])
	case "docs":
		runDocs(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(brand, version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	endpoint := fs.String("endpoint", "quic://127.0.0.1:9090", "ClutchCall gateway endpoint")
	force := fs.Bool("force", false, "Overwrite an existing target directory")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, brandify("Usage: clutch init <lang> <name> [--endpoint url] [--force]"))
		fmt.Fprintln(os.Stderr, "  lang: go | typescript | python")
		fs.PrintDefaults()
	}

	posArgs, flagArgs := splitPositional(args, 2)
	if err := fs.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	if len(posArgs) < 2 {
		fs.Usage()
		os.Exit(2)
	}

	lang, name := posArgs[0], posArgs[1]
	if err := scaffold.Init(scaffold.Options{
		Lang:     lang,
		Name:     name,
		Endpoint: *endpoint,
		Force:    *force,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "init failed:", err)
		os.Exit(1)
	}
	fmt.Printf("Scaffolded %s project at ./%s\n", lang, name)
}

func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dir := fs.String("dir", ".", "Project directory to inspect")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := migrate.Run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "migrate failed:", err)
		os.Exit(1)
	}
}

func runDocs(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, brandify("Usage: clutch docs <overview|search|get-page> [args]"))
		os.Exit(2)
	}
	if err := docs.Run(args[0], args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "docs failed:", err)
		os.Exit(1)
	}
}

var authUsage = brandify(`Usage: clutch auth <subcommand>

Subcommands:
  login [--token <jwt>] [--timeout 5m]   Authorize the CLI via the portal in your browser
                                          (or paste a token with --token).
  logout                                  Remove the saved credentials.
  whoami                                  Show the current identity.

The portal URL is taken from $CLUTCH_BASE_URL (default: https://portal.clutchcall.dev).
`)

func runAuth(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, authUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "login":
		runAuthLogin(args[1:])
	case "logout":
		runAuthLogout(args[1:])
	case "whoami":
		runAuthWhoami(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown auth subcommand: %s\n\n%s", args[0], authUsage)
		os.Exit(2)
	}
}

func runAuthLogin(args []string) {
	fs := flag.NewFlagSet("auth login", flag.ExitOnError)
	token := fs.String("token", "", "Paste a token instead of opening a browser (CI / headless)")
	timeout := fs.Duration("timeout", 5*time.Minute, "How long to wait for the browser callback")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *token != "" {
		creds, err := auth.LoginWithToken(*token)
		if err != nil {
			fmt.Fprintln(os.Stderr, "login failed:", err)
			os.Exit(1)
		}
		path, _ := auth.CredentialsPath()
		fmt.Fprintf(os.Stderr, "Saved token to %s\n", path)
		_ = creds
		return
	}

	creds, err := auth.Login(context.Background(), auth.LoginOptions{
		Timeout: *timeout,
		Stderr:  os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "login failed:", err)
		os.Exit(1)
	}
	path, _ := auth.CredentialsPath()
	fmt.Fprintf(os.Stderr, "Signed in as %s — credentials saved to %s\n", creds.Email, path)
}

func runAuthLogout(_ []string) {
	if err := auth.Logout(); err != nil {
		fmt.Fprintln(os.Stderr, "logout failed:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "Logged out.")
}

func runAuthWhoami(_ []string) {
	creds, err := auth.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "whoami failed:", err)
		os.Exit(1)
	}
	if creds == nil {
		fmt.Fprintln(os.Stderr, brandify("Not logged in. Run `clutch auth login`."))
		os.Exit(1)
	}
	fmt.Println("Base URL:    ", creds.BaseURL)
	if creds.Email != "" {
		fmt.Println("User:        ", creds.Email)
	}
	if creds.UserID != "" {
		fmt.Println("User ID:     ", creds.UserID)
	}
	if len(creds.Orgs) > 0 {
		fmt.Println("Orgs:")
		for _, o := range creds.Orgs {
			fmt.Printf("  - %s (%s)\n", o.Name, o.ID)
		}
	}
	if creds.ExpiresAt > 0 {
		exp := time.Unix(creds.ExpiresAt, 0)
		remaining := time.Until(exp).Round(time.Second)
		if remaining > 0 {
			fmt.Printf("Token expires: %s (in %s)\n", exp.Format(time.RFC3339), remaining)
		} else {
			fmt.Printf(brandify("Token expired: %s — run `clutch auth login` to refresh\n"), exp.Format(time.RFC3339))
		}
	}
}

// ─── trunks ─────────────────────────────────────────────────────────────

var trunksUsage = brandify(`Usage: clutch trunks <subcommand>

Subcommands:
  list [--org <id>] [--json]              List trunks for the active org.
  show <id|name> [--org <id>] [--json]    Show one trunk in detail.
`)

func runTrunks(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, trunksUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("trunks list", flag.ExitOnError)
		org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
		asJSON := fs.Bool("json", false, "Emit raw JSON instead of a table")
		_ = fs.Parse(args[1:])
		if err := trunks.List(context.Background(), *org, *asJSON); err != nil {
			fmt.Fprintln(os.Stderr, "trunks list failed:", err)
			os.Exit(1)
		}
	case "show":
		fs := flag.NewFlagSet("trunks show", flag.ExitOnError)
		org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
		asJSON := fs.Bool("json", false, "Emit raw JSON instead of pretty fields")
		pos, flags := splitPositional(args[1:], 1)
		_ = fs.Parse(flags)
		if len(pos) != 1 {
			fmt.Fprintln(os.Stderr, brandify("Usage: clutch trunks show <id|name> [--org <id>] [--json]"))
			os.Exit(2)
		}
		if err := trunks.Show(context.Background(), pos[0], *org, *asJSON); err != nil {
			fmt.Fprintln(os.Stderr, "trunks show failed:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown trunks subcommand: %s\n\n%s", args[0], trunksUsage)
		os.Exit(2)
	}
}

// ─── dial ───────────────────────────────────────────────────────────────

func runDial(args []string) {
	fs := flag.NewFlagSet("dial", flag.ExitOnError)
	trunk := fs.String("trunk", "", "Trunk ID to route through (required)")
	org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
	from := fs.String("from", "", "Caller-ID (E.164); must be in the trunk's allowlist")
	maxMs := fs.Int("max-ms", 0, "Hard cap on call duration in milliseconds")
	app := fs.String("app", "",
		brandify("Dialplan app on answer. Default: AI_BIDIRECTIONAL_STREAM. "+
			"List all with `clutch dial --list-apps`."))
	appArgs := fs.String("app-args", "",
		"Args for --app (AI_BIDIRECTIONAL_STREAM: agent_id; PLAYBACK: wav path; "+
			"UNPARK_AND_BRIDGE: target call_sid).")
	agent := fs.String("agent", "",
		brandify("Shortcut for --app=AI_BIDIRECTIONAL_STREAM --app-args=<agent_id>. "+
			"List your agents with `clutch agents list`."))
	wsURL := fs.String("ai-ws-url", "", "AI WebSocket URL override (AI_BIDIRECTIONAL_STREAM only)")
	quicURL := fs.String("ai-quic-url", "", "AI QUIC URL override (AI_BIDIRECTIONAL_STREAM only)")
	bargePatience := fs.Int("barge-patience-ms", 0, "How long to keep streaming TTS after barge-in detection")
	clientID := fs.String("client-id", "", "Optional correlation ID echoed back in events")
	asJSON := fs.Bool("json", false, "Emit raw JSON response")
	listApps := fs.Bool("list-apps", false, "Print supported --app values and exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, brandify("Usage: clutch dial <e164-or-sip-uri> --trunk <id> [flags]"))
		fs.PrintDefaults()
	}
	pos, flags := splitPositional(args, 1)
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}

	if *listApps {
		fmt.Println("Supported --app values (default: AI_BIDIRECTIONAL_STREAM):")
		fmt.Println()
		for _, a := range calls.DialplanApps {
			fmt.Printf("  %-26s %s\n", a.Name, a.Help)
			if a.NeedArg != "" {
				fmt.Printf("  %-26s   --app-args expects: %s\n", "", a.NeedArg)
			}
		}
		return
	}

	if len(pos) != 1 {
		fs.Usage()
		os.Exit(2)
	}

	// --agent is a friendlier alias for the AI_BIDIRECTIONAL_STREAM +
	// --app-args=<agent_id> combo most CLI users actually want. Resolve
	// it before the validation below so a single --agent counts as both.
	if *agent != "" {
		if *appArgs != "" && *appArgs != *agent {
			fmt.Fprintln(os.Stderr,
				"--agent and --app-args are both set with different values; pick one.")
			os.Exit(2)
		}
		if *app != "" && *app != "AI_BIDIRECTIONAL_STREAM" {
			fmt.Fprintln(os.Stderr,
				"--agent only makes sense with --app=AI_BIDIRECTIONAL_STREAM (the default).")
			os.Exit(2)
		}
		*app = "AI_BIDIRECTIONAL_STREAM"
		*appArgs = *agent
	}

	// AI_BIDIRECTIONAL_STREAM (the default) needs an agent_id in
	// default_app_args — without it the BFF skips agent hydration, the
	// gateway connects the carrier leg but allocates no agent_runtime
	// port, and the caller hears silence. Catch this client-side
	// instead of letting the call connect and silently fail.
	effectiveApp := *app
	if effectiveApp == "" {
		effectiveApp = "AI_BIDIRECTIONAL_STREAM"
	}
	if effectiveApp == "AI_BIDIRECTIONAL_STREAM" && *appArgs == "" {
		fmt.Fprintln(os.Stderr, brandify(
			"AI_BIDIRECTIONAL_STREAM (default) requires an agent.\n"+
				"  pass --agent <agent_id> (list with `clutch agents list`),\n"+
				"  or pick a different --app (see `clutch dial --list-apps`)."))
		os.Exit(2)
	}

	resp, err := calls.Dial(context.Background(), calls.DialOptions{
		OrgID:           *org,
		TrunkID:         *trunk,
		To:              pos[0],
		CallFrom:        *from,
		MaxDurationMs:   *maxMs,
		DefaultApp:      *app,
		DefaultAppArgs:  *appArgs,
		AIWebsocketURL:  *wsURL,
		AIQuicURL:       *quicURL,
		BargeInPatience: *bargePatience,
		ClientID:        *clientID,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial failed:", err)
		os.Exit(1)
	}
	if *asJSON {
		out, _ := jsonMarshal(resp)
		fmt.Println(out)
		return
	}
	fmt.Println("call_sid:        ", resp.CallSID)
	fmt.Println("status:          ", resp.Status)
	if resp.SessionID != 0 {
		fmt.Println("session_id:      ", resp.SessionID)
	}
	if resp.AgentConfigKey != "" {
		fmt.Println("agent_config_key:", resp.AgentConfigKey)
	}
}

// ─── hangup ─────────────────────────────────────────────────────────────

func runHangup(args []string) {
	fs := flag.NewFlagSet("hangup", flag.ExitOnError)
	org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
	pos, flags := splitPositional(args, 1)
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, brandify("Usage: clutch hangup <call_sid> [--org <id>]"))
		os.Exit(2)
	}
	if err := calls.Hangup(context.Background(), pos[0], *org); err != nil {
		fmt.Fprintln(os.Stderr, "hangup failed:", err)
		os.Exit(1)
	}
}

// ─── call-control verbs (route through ExecuteDialplan) ────────────────

func runTransfer(args []string) {
	fs := flag.NewFlagSet("transfer", flag.ExitOnError)
	org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
	pos, flags := splitPositional(args, 2)
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	if len(pos) != 2 {
		fmt.Fprintln(os.Stderr, brandify("Usage: clutch transfer <call_sid> <destination> [--org <id>]"))
		fmt.Fprintln(os.Stderr, "  destination: E.164 (+15551234567) or SIP URI (sip:user@host)")
		os.Exit(2)
	}
	if err := calls.Transfer(context.Background(), pos[0], pos[1], *org); err != nil {
		fmt.Fprintln(os.Stderr, "transfer failed:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "REFER sent.")
}

func runMuteUnmute(args []string, mute bool) {
	verb := "unmute"
	if mute {
		verb = "mute"
	}
	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
	wire := fs.Bool("wire", false, "Also send a SIP recvonly re-INVITE (carrier-visible mute)")
	pos, flags := splitPositional(args, 1)
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, brandify("Usage: clutch %s <call_sid> [--wire] [--org <id>]\n"), verb)
		os.Exit(2)
	}
	var err error
	if mute {
		err = calls.Mute(context.Background(), pos[0], *org, *wire)
	} else {
		err = calls.Unmute(context.Background(), pos[0], *org, *wire)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s failed: %s\n", verb, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "%s applied.\n", verb)
}

func runHoldUnhold(args []string, hold bool) {
	verb := "unhold"
	if hold {
		verb = "hold"
	}
	fs := flag.NewFlagSet(verb, flag.ExitOnError)
	org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
	pos, flags := splitPositional(args, 1)
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, brandify("Usage: clutch %s <call_sid> [--org <id>]\n"), verb)
		os.Exit(2)
	}
	var err error
	if hold {
		err = calls.Hold(context.Background(), pos[0], *org)
	} else {
		err = calls.Unhold(context.Background(), pos[0], *org)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s failed: %s\n", verb, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "%s applied.\n", verb)
}

func runSendDTMF(args []string) {
	fs := flag.NewFlagSet("send-dtmf", flag.ExitOnError)
	org := fs.String("org", "", "Org ID (defaults to first org in saved credentials)")
	mode := fs.String("mode", "rfc2833", "DTMF mode: rfc2833 | info")
	durationMs := fs.Int("duration-ms", 160, "DTMF tone duration in ms (40–4000; BFF default 160)")
	pos, flags := splitPositional(args, 2)
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	if len(pos) != 2 {
		fmt.Fprintln(os.Stderr, brandify("Usage: clutch send-dtmf <call_sid> <digit> [--mode rfc2833|info] [--duration-ms 160]"))
		os.Exit(2)
	}
	if err := calls.SendDTMF(context.Background(), pos[0], *org, pos[1], *mode, *durationMs); err != nil {
		fmt.Fprintln(os.Stderr, "send-dtmf failed:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "sent %s (%s, %dms)\n", pos[1], *mode, *durationMs)
	fmt.Fprintln(os.Stderr, "Terminated.")
}

func jsonMarshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// splitPositional pulls up to n positional args from the head, returning (positional, remainingFlags).
// Stops collecting positionals at the first arg that begins with '-'.
func splitPositional(args []string, n int) ([]string, []string) {
	pos := []string{}
	i := 0
	for i < len(args) && len(pos) < n {
		if len(args[i]) > 0 && args[i][0] == '-' {
			break
		}
		pos = append(pos, args[i])
		i++
	}
	return pos, args[i:]
}
