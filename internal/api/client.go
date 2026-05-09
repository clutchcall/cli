// Package api is a small tRPC-over-HTTP client. The BFF mounts tRPC at
// `/trpc/*` and uses the no-transformer wire shape (initTRPC.create()
// with no `transformer:` option):
//
//   query:    GET  /trpc/<router>.<proc>?input=<urlencoded JSON of input>
//   mutation: POST /trpc/<router>.<proc>  body=<JSON of input>
//   response: {"result":{"data": <output>}}
//             or {"error":{"message":"…","code":N,"data":{...}}}
//
// (If the BFF later adds superjson, the input/output get wrapped in a
//  {"json": …} envelope. We don't handle that here yet.)
//
// Auth: `Authorization: Bearer <access_token>` from internal/auth.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/clutchcall/cli/internal/auth"
)

// mintTraceparent returns a fresh W3C `traceparent` header value.
// `00-<32 hex trace_id>-<16 hex span_id>-<flags>`. Sampled flag (01) is
// always set so SigNoz keeps the trace; the engine's tail-based sampler
// can drop it later if the per-tenant sampling rate is below 100%.
func mintTraceparent() string {
	var traceID [16]byte
	var spanID [8]byte
	if _, err := rand.Read(traceID[:]); err != nil {
		// crypto/rand failure on Linux is fatal kernel state; fall back to
		// a static traceparent rather than skip the header entirely.
		return "00-00000000000000000000000000000000-0000000000000000-01"
	}
	_, _ = rand.Read(spanID[:])
	return "00-" + hex.EncodeToString(traceID[:]) + "-" + hex.EncodeToString(spanID[:]) + "-01"
}

type Client struct {
	BaseURL    string
	Token      string
	HTTP       *http.Client
}

// NewFromCreds builds a client from saved credentials. If creds is nil,
// the client still works for unauthenticated procedures (none of which
// orgProcedure-protected calls in this CLI use, but the failure is then
// the server's authoritative 401 rather than a local "not logged in").
func NewFromCreds(creds *auth.Credentials) *Client {
	c := &Client{
		BaseURL: auth.BaseURL(),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	if creds != nil {
		if creds.BaseURL != "" {
			c.BaseURL = creds.BaseURL
		}
		c.Token = creds.AccessToken
	}
	return c
}

// MustLoadClient is the convenience used by subcommands. Errors out the
// process if credentials are missing — the user must `clutch auth login`
// first for any of the orgProcedure-protected endpoints.
func MustLoadClient() *Client {
	creds, err := auth.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth error:", err)
		os.Exit(1)
	}
	if creds == nil || creds.AccessToken == "" {
		fmt.Fprintln(os.Stderr, "Not logged in. Run `clutch auth login` first.")
		os.Exit(1)
	}
	return NewFromCreds(creds)
}

// ─── tRPC envelope shapes (no-transformer) ─────────────────────────────

type rpcResponse struct {
	Result *struct {
		Data json.RawMessage `json:"data"`
	} `json:"result,omitempty"`
	Error *struct {
		Message string          `json:"message"`
		Code    int             `json:"code"`
		Data    json.RawMessage `json:"data"`
	} `json:"error,omitempty"`
}

// Query performs a tRPC query (GET).
func (c *Client) Query(ctx context.Context, proc string, input any, out any) error {
	return c.do(ctx, http.MethodGet, proc, input, out)
}

// Mutate performs a tRPC mutation (POST).
func (c *Client) Mutate(ctx context.Context, proc string, input any, out any) error {
	return c.do(ctx, http.MethodPost, proc, input, out)
}

func (c *Client) do(ctx context.Context, method, proc string, input, out any) error {
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/trpc/" + proc

	rawInput := mustJSON(input)

	var (
		req *http.Request
		err error
	)
	switch method {
	case http.MethodGet:
		u, _ := url.Parse(endpoint)
		q := u.Query()
		q.Set("input", string(rawInput))
		u.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, method, u.String(), nil)
	case http.MethodPost:
		req, err = http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(rawInput))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	default:
		return fmt.Errorf("unsupported method %q", method)
	}
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	// W3C traceparent — every CLI command becomes a trace whose root is
	// "client.cli" and whose first child is the BFF's request span, which
	// then parents the engine's call.outbound. CLUTCH_TRACEPARENT env
	// override lets a parent harness (CI runner, integration test) thread
	// its own trace through multiple clutch invocations.
	if tp := os.Getenv("CLUTCH_TRACEPARENT"); tp != "" {
		req.Header.Set("traceparent", tp)
	} else {
		req.Header.Set("traceparent", mintTraceparent())
	}

	debug := os.Getenv("CLUTCH_DEBUG") == "1"
	if debug {
		fmt.Fprintf(os.Stderr, "[clutch] %s %s\n", method, req.URL.String())
		if method == http.MethodPost {
			fmt.Fprintf(os.Stderr, "[clutch] body: %s\n", truncate(string(rawInput), 500))
		}
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[clutch] HTTP %d, body: %s\n",
			resp.StatusCode, truncate(string(body), 1000))
	}

	// 401/403 are common enough that we surface a friendly message rather
	// than dumping raw HTML/JSON.
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("unauthorized — your token may have expired; run `clutch auth login` again")
	}

	var env rpcResponse
	if err := json.Unmarshal(body, &env); err != nil {
		// Not a tRPC envelope (could be a 500 HTML page, a CDN intercept,
		// a non-JSON response). Surface enough to diagnose.
		return fmt.Errorf("%s: HTTP %d non-tRPC response: %s",
			proc, resp.StatusCode, truncate(string(body), 300))
	}
	if env.Error != nil {
		msg := env.Error.Message
		if msg == "" {
			rawErr, _ := json.Marshal(env.Error)
			msg = "(empty message; raw error: " + truncate(string(rawErr), 400) + ")"
		}
		if env.Error.Code != 0 {
			return fmt.Errorf("%s: %s [code %d, HTTP %d]",
				proc, msg, env.Error.Code, resp.StatusCode)
		}
		return fmt.Errorf("%s: %s [HTTP %d]", proc, msg, resp.StatusCode)
	}
	if env.Result == nil {
		return fmt.Errorf("%s: malformed tRPC response (HTTP %d): %s",
			proc, resp.StatusCode, truncate(string(body), 300))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result.Data, out)
}

// ─── helpers ──────────────────────────────────────────────────────────

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		// We marshal CLI-controlled structs; failing here is a programmer error.
		panic(fmt.Errorf("api: marshal input: %w", err))
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
