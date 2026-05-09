// Package trunks implements `clutch trunks {list,show}`. Backed by the
// BFF's tRPC `admin.listTrunks` procedure.
package trunks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/clutchcall/cli/internal/api"
	"github.com/clutchcall/cli/internal/auth"
)

// Trunk mirrors the shape stored in Redis under `clutchcall:trunks_data`,
// with `trunk_id` spread in by the BFF (see admin.ts:listTrunks).
type Trunk struct {
	TrunkID                  string   `json:"trunk_id"`
	DisplayName              string   `json:"display_name"`
	TenantID                 string   `json:"tenant_id,omitempty"`
	ChannelLimit             int      `json:"channel_limit,omitempty"`

	// SIP / RTP wiring
	InternalSipIP            string   `json:"internal_sip_ip,omitempty"`
	ExternalSipIP            string   `json:"external_sip_ip,omitempty"`
	InternalRtpIP            string   `json:"internal_rtp_ip,omitempty"`
	ExternalRtpIP            string   `json:"external_rtp_ip,omitempty"`
	GatewayIP                string   `json:"gateway_ip,omitempty"`
	SbcIP                    string   `json:"sbc_ip,omitempty"`
	SourcePbxPort            int      `json:"source_pbx_port,omitempty"`
	DestinationSbcPort       int      `json:"destination_sbc_port,omitempty"`
	Domain                   string   `json:"domain,omitempty"`
	Proxy                    string   `json:"proxy,omitempty"`
	RequireRegistration      bool     `json:"require_registration,omitempty"`
	SipUsername              string   `json:"sip_username,omitempty"`
	// SipPassword + ApiBearerToken intentionally NOT surfaced in any view.
	RegisterExpiresSec       int      `json:"register_expires_sec,omitempty"`
	CodecPreferences         []string `json:"codec_preferences,omitempty"`
	RtpStartPort             int      `json:"rtp_start_port,omitempty"`
	RtpEndPort               int      `json:"rtp_end_port,omitempty"`

	// Inbound handling
	RejectAudioFile          string   `json:"reject_audio_file,omitempty"`
	InboundWebhookURL        string   `json:"inbound_webhook_url,omitempty"`
	InboundAiWebsocketURL    string   `json:"inbound_ai_websocket_url,omitempty"`
	InboundAiQuicURL         string   `json:"inbound_ai_quic_url,omitempty"`

	// Barge-in defaults
	AutoBargeinMode          string   `json:"auto_bargein_mode,omitempty"`
	AutoBargeinAggressiveness int     `json:"auto_bargein_aggressiveness,omitempty"`
}

// listTrunksInput matches the orgProcedure shape: `{orgId: string}`.
type listTrunksInput struct {
	OrgID string `json:"orgId"`
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

// List fetches all trunks for the given org (default: first org in creds).
func List(ctx context.Context, orgOverride string, asJSON bool) error {
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
	var raw json.RawMessage
	if err := c.Query(ctx, "admin.listTrunks", listTrunksInput{OrgID: orgID}, &raw); err != nil {
		return err
	}

	var trunks []Trunk
	if err := json.Unmarshal(raw, &trunks); err != nil {
		// Fall back: maybe the BFF wraps in `{trunks: [...]}` — try that.
		var wrapped struct {
			Trunks []Trunk `json:"trunks"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return fmt.Errorf("decode trunks: %w", err)
		}
		trunks = wrapped.Trunks
	}

	if asJSON {
		out, _ := json.MarshalIndent(trunks, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	if len(trunks) == 0 {
		fmt.Fprintf(os.Stderr,
			"No trunks returned for org %s.\n"+
				"  • The BFF filters by tenant_id matching this orgId; trunks with "+
				"a different tenant_id (or stored without one) won't appear here.\n"+
				"  • Re-run with CLUTCH_DEBUG=1 to see the raw BFF response.\n"+
				"  • Add one with `clutch trunks add` (once implemented) or via the portal.\n",
			orgID)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSBC\tUSERNAME\tCODECS\tCHANS\tBARGE-IN")
	for _, t := range trunks {
		codecs := joinOrDash(t.CodecPreferences)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			dashIfEmpty(t.TrunkID),
			dashIfEmpty(t.DisplayName),
			dashIfEmpty(formatSbc(t)),
			dashIfEmpty(t.SipUsername),
			codecs,
			t.ChannelLimit,
			dashIfEmpty(t.AutoBargeinMode),
		)
	}
	return tw.Flush()
}

// formatSbc surfaces whichever IP/port combination is set on the trunk so
// the table has a useful "where does this trunk point" column.
func formatSbc(t Trunk) string {
	host := t.SbcIP
	if host == "" {
		host = t.GatewayIP
	}
	if host == "" {
		host = t.ExternalSipIP
	}
	if host == "" {
		return ""
	}
	if t.DestinationSbcPort > 0 {
		return fmt.Sprintf("%s:%d", host, t.DestinationSbcPort)
	}
	return host
}

// Show prints a single trunk in detail. Falls back to listing all + filtering
// (since there's no get-one endpoint yet on the BFF).
func Show(ctx context.Context, trunkID, orgOverride string, asJSON bool) error {
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
	var raw json.RawMessage
	if err := c.Query(ctx, "admin.listTrunks", listTrunksInput{OrgID: orgID}, &raw); err != nil {
		return err
	}
	var trunks []Trunk
	if err := json.Unmarshal(raw, &trunks); err != nil {
		var wrapped struct {
			Trunks []Trunk `json:"trunks"`
		}
		_ = json.Unmarshal(raw, &wrapped)
		trunks = wrapped.Trunks
	}

	for _, t := range trunks {
		if t.TrunkID == trunkID || t.DisplayName == trunkID {
			if asJSON {
				out, _ := json.MarshalIndent(t, "", "  ")
				fmt.Println(string(out))
				return nil
			}
			fmt.Println("ID:                 ", dashIfEmpty(t.TrunkID))
			fmt.Println("Display name:       ", dashIfEmpty(t.DisplayName))
			fmt.Println("Tenant:             ", dashIfEmpty(t.TenantID))
			fmt.Println("Channel limit:      ", t.ChannelLimit)
			fmt.Println()
			fmt.Println("SIP")
			fmt.Println("  SBC IP:           ", dashIfEmpty(t.SbcIP))
			fmt.Println("  Gateway IP:       ", dashIfEmpty(t.GatewayIP))
			fmt.Println("  Source PBX port:  ", t.SourcePbxPort)
			fmt.Println("  Dest SBC port:    ", t.DestinationSbcPort)
			fmt.Println("  Domain:           ", dashIfEmpty(t.Domain))
			fmt.Println("  Proxy:            ", dashIfEmpty(t.Proxy))
			fmt.Println("  Require register: ", t.RequireRegistration)
			fmt.Println("  Username:         ", dashIfEmpty(t.SipUsername))
			fmt.Println("  Register expires: ", fmt.Sprintf("%ds", t.RegisterExpiresSec))
			fmt.Println()
			fmt.Println("Media")
			fmt.Println("  Codec prefs:      ", joinOrDash(t.CodecPreferences))
			fmt.Println("  RTP port range:   ", fmt.Sprintf("%d–%d", t.RtpStartPort, t.RtpEndPort))
			fmt.Println()
			fmt.Println("Inbound handling")
			fmt.Println("  Webhook URL:      ", dashIfEmpty(t.InboundWebhookURL))
			fmt.Println("  AI WS URL:        ", dashIfEmpty(t.InboundAiWebsocketURL))
			fmt.Println("  AI QUIC URL:      ", dashIfEmpty(t.InboundAiQuicURL))
			fmt.Println("  Reject audio:     ", dashIfEmpty(t.RejectAudioFile))
			fmt.Println()
			fmt.Println("Barge-in")
			fmt.Println("  Mode:             ", dashIfEmpty(t.AutoBargeinMode))
			fmt.Println("  Aggressiveness:   ", t.AutoBargeinAggressiveness)
			return nil
		}
	}
	return fmt.Errorf("trunk %q not found in org %s", trunkID, orgID)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOrDash(ss []string) string {
	if len(ss) == 0 {
		return "-"
	}
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
