// Package calls implements `clutch dial` and `clutch hangup`. Both wrap
// tRPC mutations on the BFF (telephony.originate / telephony.terminate).
package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/clutchcall/cli/internal/api"
	"github.com/clutchcall/cli/internal/auth"
)

// DialplanApp mirrors clutchcall/api/clutchcall_types.hh::DialplanAction
// and the BFF's z.enum(DIALPLAN_ACTIONS) on telephony.originate.
type DialplanApp struct {
	Name    string
	Help    string
	NeedArg string // optional hint about what --app-args expects
}

// DialplanApps is the canonical list. Order matches the engine enum at
// clutchcall/api/clutchcall_types.hh::DialplanAction. Values 0-6 are
// usable as `--app` on dial; values 7-12 are call-control verbs only
// valid against an active call_sid (use the dedicated subcommands:
// `clutch transfer|mute|unmute|hold|unhold|send-dtmf`).
var DialplanApps = []DialplanApp{
	{"AI_BIDIRECTIONAL_STREAM", "Bridge the answered call to an AI agent (ASR → LLM → TTS).", "agent_id"},
	{"PLAYBACK", "Play an audio file then hang up (or wait for further commands).", "absolute path to .wav"},
	{"PARK", "Hold the call in silence; bridge it later via Barge / Bridge RPC.", ""},
	{"MUSIC_ON_HOLD", "Play hold music until something else bridges or hangs up.", ""},
	{"UNPARK_AND_BRIDGE", "Pull a parked call out and bridge to a new destination.", "destination call_sid"},
	{"ANSWER", "Send 200 OK and keep the leg open without playing anything.", ""},
	{"HANGUP", "Reject the call (carrier sees BYE / 4xx) without ever bridging.", ""},
	// Call-control verbs (require an active call_sid; not valid as --app on dial):
	{"TRANSFER", "[mid-call] RFC 3515 SIP REFER. Use `clutch transfer <sid> <dst>`.", ""},
	{"MUTE", "[mid-call] Silence TX. Use `clutch mute <sid>`.", ""},
	{"UNMUTE", "[mid-call] Resume TX. Use `clutch unmute <sid>`.", ""},
	{"HOLD", "[mid-call] sendrecv → sendonly re-INVITE. Use `clutch hold <sid>`.", ""},
	{"UNHOLD", "[mid-call] Resume hold. Use `clutch unhold <sid>`.", ""},
	{"SEND_DTMF", "[mid-call] Send DTMF digit. Use `clutch send-dtmf <sid> <digit>`.", ""},
}

func DialplanAppNames() []string {
	out := make([]string, len(DialplanApps))
	for i, a := range DialplanApps {
		out[i] = a.Name
	}
	return out
}

// midCallVerbs are not valid as `--app` on Dial — they require an
// active call_sid and route through ExecuteDialplan.
var midCallVerbs = map[string]bool{
	"TRANSFER": true, "MUTE": true, "UNMUTE": true,
	"HOLD": true, "UNHOLD": true, "SEND_DTMF": true,
}

// validateApp returns nil if app is empty (the BFF defaults to
// AI_BIDIRECTIONAL_STREAM) or matches one of the dialable apps. The
// mid-call verbs (TRANSFER/MUTE/etc.) are deliberately rejected here —
// use the dedicated `clutch transfer|mute|...` subcommands instead.
func validateApp(app string) error {
	if app == "" {
		return nil
	}
	if midCallVerbs[app] {
		return fmt.Errorf("%s is a mid-call verb, not a dial app — use `clutch %s <call_sid>` against an active call",
			app, strings.ToLower(strings.ReplaceAll(app, "_", "-")))
	}
	for _, a := range DialplanApps {
		if a.Name == app && !midCallVerbs[a.Name] {
			return nil
		}
	}
	return fmt.Errorf("unknown --app %q. Dialable apps: %s",
		app, strings.Join(dialableAppNames(), ", "))
}

func dialableAppNames() []string {
	out := []string{}
	for _, a := range DialplanApps {
		if !midCallVerbs[a.Name] {
			out = append(out, a.Name)
		}
	}
	return out
}

type DialOptions struct {
	OrgID            string
	TrunkID          string
	To               string
	CallFrom         string
	MaxDurationMs    int
	DefaultApp       string // "AI_BIDIRECTIONAL_STREAM" | "PLAYBACK" | "ECHO" | …
	DefaultAppArgs   string
	AIWebsocketURL   string
	AIQuicURL        string
	AutoBargeIn      *bool
	BargeInPatience  int
	ClientID         string
}

type originateInput struct {
	OrgID                string `json:"orgId"`
	TrunkID              string `json:"trunk_id"`
	To                   string `json:"to"`
	CallFrom             string `json:"call_from,omitempty"`
	DefaultApp           string `json:"default_app,omitempty"`
	DefaultAppArgs       string `json:"default_app_args,omitempty"`
	AIWebsocketURL       string `json:"ai_websocket_url,omitempty"`
	AIQuicURL            string `json:"ai_quic_url,omitempty"`
	MaxDurationMs        int    `json:"max_duration_ms,omitempty"`
	AutoBargeIn          *bool  `json:"auto_barge_in,omitempty"`
	BargeInPatienceMs    int    `json:"barge_in_patience_ms,omitempty"`
	ClientID             string `json:"client_id,omitempty"`
}

// OriginateResp mirrors the BFF's response shape (see telephony.ts:80-89).
type OriginateResp struct {
	CallSID        string `json:"call_sid"`
	Status         string `json:"status"`
	ErrorMessage   string `json:"error_message,omitempty"`
	ErrorCode      int    `json:"error_code,omitempty"`
	TimestampMs    int64  `json:"timestamp_ms,omitempty"`
	SessionID      uint32 `json:"session_id,omitempty"`
	AgentConfigKey string `json:"agent_config_key,omitempty"`
}

func resolveOrgID(creds *auth.Credentials, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if len(creds.Orgs) == 0 {
		return "", fmt.Errorf(
			"no orgs in saved credentials — either pass --org <id> explicitly, " +
				"or re-run `clutch auth login` (older credentials may have been " +
				"saved before tenant data was loaded)")
	}
	return creds.Orgs[0].ID, nil
}

// Dial issues an Originate to place a call.
func Dial(ctx context.Context, opt DialOptions) (*OriginateResp, error) {
	creds, err := auth.Load()
	if err != nil {
		return nil, err
	}
	if creds == nil {
		return nil, fmt.Errorf("not logged in — run `clutch auth login`")
	}
	orgID, err := resolveOrgID(creds, opt.OrgID)
	if err != nil {
		return nil, err
	}
	if opt.TrunkID == "" {
		return nil, fmt.Errorf("trunk id is required (--trunk <id>); list with `clutch trunks list`")
	}
	if opt.To == "" {
		return nil, fmt.Errorf("destination is required (positional E.164 / SIP URI)")
	}
	if err := validateApp(opt.DefaultApp); err != nil {
		return nil, err
	}

	in := originateInput{
		OrgID:             orgID,
		TrunkID:           opt.TrunkID,
		To:                opt.To,
		CallFrom:          opt.CallFrom,
		DefaultApp:        opt.DefaultApp,
		DefaultAppArgs:    opt.DefaultAppArgs,
		AIWebsocketURL:    opt.AIWebsocketURL,
		AIQuicURL:         opt.AIQuicURL,
		MaxDurationMs:     opt.MaxDurationMs,
		AutoBargeIn:       opt.AutoBargeIn,
		BargeInPatienceMs: opt.BargeInPatience,
		ClientID:          opt.ClientID,
	}

	c := api.NewFromCreds(creds)
	var resp OriginateResp
	if err := c.Mutate(ctx, "telephony.originate", in, &resp); err != nil {
		return nil, err
	}
	if resp.ErrorMessage != "" {
		return &resp, fmt.Errorf("gateway rejected dial: %s (code %d)", resp.ErrorMessage, resp.ErrorCode)
	}
	return &resp, nil
}

// callBffMutation is the shared shim for the per-verb BFF procs
// (telephony.transfer / .mute / .hold / .unhold / .sendDtmf). Each
// proc is orgProcedure-protected, takes the call_sid + verb-specific
// args, and returns a status JSON we don't currently need to surface.
func callBffMutation(ctx context.Context, proc, orgOverride string, extra map[string]any) error {
	creds, err := auth.Load()
	if err != nil {
		return err
	}
	if creds == nil {
		return fmt.Errorf("not logged in — run `clutch auth login`")
	}
	orgID, err := resolveOrgID(creds, orgOverride)
	if err != nil {
		return err
	}
	in := map[string]any{"orgId": orgID}
	for k, v := range extra {
		in[k] = v
	}
	c := api.NewFromCreds(creds)
	var resp json.RawMessage
	return c.Mutate(ctx, proc, in, &resp)
}

// Transfer sends a SIP REFER (RFC 3515) on the active leg via
// telephony.transfer.
func Transfer(ctx context.Context, callSID, destination, orgOverride string) error {
	if destination == "" {
		return fmt.Errorf("destination is required (E.164 or sip:user@host)")
	}
	return callBffMutation(ctx, "telephony.transfer", orgOverride, map[string]any{
		"call_sid":    callSID,
		"destination": destination,
	})
}

// Mute calls telephony.mute with on=true. If wireLevel is true, the BFF
// escalates to a SIP recvonly re-INVITE so the carrier sees the mute.
func Mute(ctx context.Context, callSID, orgOverride string, wireLevel bool) error {
	return callBffMutation(ctx, "telephony.mute", orgOverride, map[string]any{
		"call_sid":   callSID,
		"on":         true,
		"wire_level": wireLevel,
	})
}

// Unmute calls telephony.mute with on=false (the BFF collapses
// mute/unmute into one proc with a boolean).
func Unmute(ctx context.Context, callSID, orgOverride string, wireLevel bool) error {
	return callBffMutation(ctx, "telephony.mute", orgOverride, map[string]any{
		"call_sid":   callSID,
		"on":         false,
		"wire_level": wireLevel,
	})
}

func Hold(ctx context.Context, callSID, orgOverride string) error {
	return callBffMutation(ctx, "telephony.hold", orgOverride, map[string]any{
		"call_sid": callSID,
	})
}

func Unhold(ctx context.Context, callSID, orgOverride string) error {
	return callBffMutation(ctx, "telephony.unhold", orgOverride, map[string]any{
		"call_sid": callSID,
	})
}

// SendDTMF calls telephony.sendDtmf. The BFF accepts mode = rfc2833 | info
// (no in-band on the BFF surface — engine emits it from inband if requested
// but the BFF doesn't expose that switch). Duration: 40–4000 ms.
func SendDTMF(ctx context.Context, callSID, orgOverride, digit, mode string, durationMs int) error {
	if len(digit) != 1 || !strings.Contains("0123456789*#ABCD", digit) {
		return fmt.Errorf("invalid DTMF digit %q (expect one of 0-9, *, #, A-D)", digit)
	}
	switch mode {
	case "rfc2833", "info":
	default:
		return fmt.Errorf("invalid DTMF mode %q (BFF accepts rfc2833 | info)", mode)
	}
	if durationMs < 40 || durationMs > 4000 {
		return fmt.Errorf("DTMF duration %dms out of range (40–4000)", durationMs)
	}
	return callBffMutation(ctx, "telephony.sendDtmf", orgOverride, map[string]any{
		"call_sid":    callSID,
		"digit":       digit,
		"mode":        mode,
		"duration_ms": durationMs,
	})
}

// Hangup terminates an active call by call_sid.
func Hangup(ctx context.Context, callSID, orgOverride string) error {
	creds, err := auth.Load()
	if err != nil {
		return err
	}
	if creds == nil {
		return fmt.Errorf("not logged in — run `clutch auth login`")
	}
	orgID, err := resolveOrgID(creds, orgOverride)
	if err != nil {
		return err
	}

	c := api.NewFromCreds(creds)
	var resp json.RawMessage
	in := map[string]string{"orgId": orgID, "call_sid": callSID}
	if err := c.Mutate(ctx, "telephony.terminate", in, &resp); err != nil {
		return err
	}
	return nil
}
