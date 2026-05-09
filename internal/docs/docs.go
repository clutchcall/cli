// Package docs implements the `clutch docs` subcommand.
//
// Backed by the live Mintlify-built docs site at https://docs.clutchcall.dev:
//
//   overview      → GET /llms.txt           (table of contents for LLM triage)
//   search <q>    → POST /api/mcp           (Streamable-HTTP MCP, tools/call search_docs)
//   get-page <s>  → GET /<slug>.md          (per-page Markdown mirror)
//
// The base URL is overridable via $CLUTCH_DOCS_BASE so the same binary can
// point at the telequick deploy or a local Mintlify dev server.
package docs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultBase = "https://docs.clutchcall.dev"

func base() string {
	if v := os.Getenv("CLUTCH_DOCS_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultBase
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// Run dispatches to the right docs subcommand.
func Run(sub string, args []string) error {
	switch sub {
	case "overview":
		return overview()
	case "search":
		if len(args) == 0 {
			return fmt.Errorf("usage: clutch docs search <query>")
		}
		return search(strings.Join(args, " "))
	case "get-page":
		if len(args) == 0 {
			return fmt.Errorf("usage: clutch docs get-page <slug>")
		}
		return getPage(args[0])
	default:
		return fmt.Errorf("unknown docs subcommand %q (try: overview, search, get-page)", sub)
	}
}

// ─── overview ───────────────────────────────────────────────────────────────

func overview() error {
	body, err := httpGet(base() + "/llms.txt")
	if err != nil {
		return err
	}
	os.Stdout.Write(body)
	if !bytes.HasSuffix(body, []byte("\n")) {
		fmt.Println()
	}
	return nil
}

// ─── get-page ───────────────────────────────────────────────────────────────

func getPage(slug string) error {
	clean := strings.Trim(slug, "/")
	clean = strings.TrimSuffix(clean, ".md")
	if clean == "" {
		return fmt.Errorf("empty slug")
	}
	// Reject anything that isn't a normal slug — paranoia about command
	// substitution / CRLF injection in pipe-to-pager flows.
	for _, r := range clean {
		if !(r == '/' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("invalid slug %q", slug)
		}
	}
	body, err := httpGet(base() + "/" + clean + ".md")
	if err != nil {
		return err
	}
	os.Stdout.Write(body)
	if !bytes.HasSuffix(body, []byte("\n")) {
		fmt.Println()
	}
	return nil
}

// ─── search ─────────────────────────────────────────────────────────────────
//
// Talks to /api/mcp via JSON-RPC tools/call. We could shortcut by pulling
// /search-index.json and BM25-ing locally, but going through MCP gives the
// CLI the same surface a coding agent would use — fewer code paths to keep
// in sync if scoring ever changes server-side.

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCallResult struct {
	Content []contentBlock `json:"content"`
}

func search(query string) error {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "search_docs",
			"arguments": map[string]any{
				"query": query,
				"limit": 8,
			},
		},
	}
	buf, err := json.Marshal(req)
	if err != nil {
		return err
	}

	endpoint, err := url.JoinPath(base(), "api", "mcp")
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: HTTP %d: %s", endpoint, resp.StatusCode, truncate(string(body), 200))
	}

	var rpc rpcResp
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return fmt.Errorf("decode rpc response: %w", err)
	}
	if rpc.Error != nil {
		return fmt.Errorf("MCP error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	var tc toolCallResult
	if err := json.Unmarshal(rpc.Result, &tc); err != nil {
		return fmt.Errorf("decode tool result: %w", err)
	}
	for _, b := range tc.Content {
		if b.Type == "text" {
			fmt.Println(b.Text)
		}
	}
	return nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

func httpGet(rawURL string) ([]byte, error) {
	resp, err := httpClient().Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
