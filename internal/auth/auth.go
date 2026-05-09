// Package auth implements `clutch auth {login,logout,whoami}`.
//
// Login flow (browser-callback):
//
//	1. `clutch auth login` picks a random localhost port and starts a tiny
//	   HTTP server with a /cb handler.
//	2. Opens the user's browser to ${CLUTCH_BASE_URL}/cli-auth?port=<N>&state=<S>.
//	3. The portal page (frontend's CliAuth component) lets the user sign in
//	   via the existing GoTrue flow, then POSTs the Supabase session JSON
//	   to http://127.0.0.1:<N>/cb.
//	4. CLI saves the session under $XDG_CONFIG_HOME/clutch/credentials.json
//	   and shuts down the loopback server.
//
// Endpoints used by other subcommands (dial, trunks, …) read the saved
// access_token via Token() and add it as `Authorization: Bearer <token>`
// on tRPC calls.
//
// Override the portal URL with $CLUTCH_BASE_URL (default: production).
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://portal.clutchcall.dev"
	envBaseURL     = "CLUTCH_BASE_URL"
	envToken       = "CLUTCH_TOKEN"
)

// BaseURL returns the portal/BFF base URL the CLI talks to.
// Honors $CLUTCH_BASE_URL; falls back to the production portal.
func BaseURL() string {
	if v := os.Getenv(envBaseURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultBaseURL
}

// Credentials is what we serialize to disk. The Supabase session JSON is
// stored as-is so refresh + expiry handling stays straightforward.
type Credentials struct {
	BaseURL      string `json:"base_url"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // unix seconds
	TokenType    string `json:"token_type,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	Email        string `json:"email,omitempty"`
	Orgs         []Org  `json:"orgs,omitempty"`
	SavedAt      int64  `json:"saved_at"` // unix seconds
}

type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

// CredentialsPath returns the on-disk path. Resolution order:
//   1. $CLUTCH_CONFIG_DIR/credentials.json (operator override)
//   2. os.UserConfigDir()/clutch/credentials.json — cross-platform:
//        Linux:   $XDG_CONFIG_HOME/clutch/credentials.json
//                 (or $HOME/.config/clutch/credentials.json)
//        macOS:   $HOME/Library/Application Support/clutch/credentials.json
//        Windows: %AppData%\clutch\credentials.json
func CredentialsPath() (string, error) {
	if v := os.Getenv("CLUTCH_CONFIG_DIR"); v != "" {
		return filepath.Join(v, "credentials.json"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "clutch", "credentials.json"), nil
}

// Load reads saved credentials. Returns nil, nil if no credentials file
// exists (caller should treat as "not logged in"). $CLUTCH_TOKEN, if set,
// short-circuits with a synthetic in-memory credential.
func Load() (*Credentials, error) {
	if t := os.Getenv(envToken); t != "" {
		return &Credentials{
			BaseURL:     BaseURL(),
			AccessToken: t,
			TokenType:   "bearer",
			SavedAt:     time.Now().Unix(),
		}, nil
	}
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// Save writes credentials with 0600 permissions.
func Save(c *Credentials) error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	c.SavedAt = time.Now().Unix()
	buf, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return err
	}
	return nil
}

// Logout removes the saved credentials file. No-op if it doesn't exist.
func Logout() error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ─── Browser-callback login ────────────────────────────────────────────

// Login spins up a localhost HTTP server, opens the portal's /cli-auth
// page in the user's browser, and waits for the resulting POST. Saves
// the credentials and returns them.
func Login(ctx context.Context, opts LoginOptions) (*Credentials, error) {
	if opts.NoBrowser {
		return nil, errors.New("--no-browser is not yet supported; run from a desktop or pass --token")
	}

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("rand: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	// Bind on 127.0.0.1:0 so the kernel picks a free port for us.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("loopback listen: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	type result struct {
		creds *Credentials
		err   error
	}
	resultCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/cb", func(w http.ResponseWriter, r *http.Request) {
		// CORS preflight from the browser (no-cors fetch sends preflight
		// for content-type: application/json).
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			resultCh <- result{nil, fmt.Errorf("read /cb body: %w", err)}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    int64  `json:"expires_at"`
			TokenType    string `json:"token_type"`
			UserID       string `json:"user_id"`
			Email        string `json:"email"`
			Orgs         []Org  `json:"orgs"`
			State        string `json:"state"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			resultCh <- result{nil, fmt.Errorf("parse /cb body: %w", err)}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.State != state {
			resultCh <- result{nil, errors.New("state mismatch — possible cross-flow replay")}
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if payload.AccessToken == "" {
			resultCh <- result{nil, errors.New("no access_token in /cb body")}
			http.Error(w, "missing access_token", http.StatusBadRequest)
			return
		}
		creds := &Credentials{
			BaseURL:      BaseURL(),
			AccessToken:  payload.AccessToken,
			RefreshToken: payload.RefreshToken,
			ExpiresAt:    payload.ExpiresAt,
			TokenType:    payload.TokenType,
			UserID:       payload.UserID,
			Email:        payload.Email,
			Orgs:         payload.Orgs,
		}
		resultCh <- result{creds, nil}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = server.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := fmt.Sprintf("%s/cli-auth?port=%d&state=%s", BaseURL(), port, state)
	fmt.Fprintln(opts.Stderr, "Opening browser to authorize the CLI…")
	fmt.Fprintln(opts.Stderr, "If your browser didn't open, visit:")
	fmt.Fprintln(opts.Stderr, "  ", authURL)

	if err := openBrowser(authURL); err != nil {
		fmt.Fprintln(opts.Stderr, "(couldn't auto-open browser:", err, ")")
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	select {
	case res := <-resultCh:
		if res.err != nil {
			return nil, res.err
		}
		if err := Save(res.creds); err != nil {
			return nil, fmt.Errorf("save credentials: %w", err)
		}
		return res.creds, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out waiting for browser auth (%s)", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type LoginOptions struct {
	Timeout   time.Duration
	NoBrowser bool
	Stderr    io.Writer
}

// LoginWithToken saves a manually-supplied token (for headless CI / first
// boot before a browser is available).
func LoginWithToken(token string) (*Credentials, error) {
	if token == "" {
		return nil, errors.New("empty token")
	}
	creds := &Credentials{
		BaseURL:     BaseURL(),
		AccessToken: token,
		TokenType:   "bearer",
	}
	if err := Save(creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// ─── helpers ───────────────────────────────────────────────────────────

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
