package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoChatGPTToken means auth.json holds no ChatGPT access token (e.g. API key
// login, or credentials kept in the OS keyring), so there is nothing to refresh.
var ErrNoChatGPTToken = errors.New("no ChatGPT access token in auth.json")

// tokenRefreshTimeout bounds one `codex app-server` refresh run.
const tokenRefreshTimeout = time.Minute

// RunTokenRefresher enforces codex.token_refresh until ctx is done: it checks
// the login immediately (catching up on checks missed while stopped) and then
// every day at token_refresh.at local time. It returns immediately when the
// check is disabled.
//
// Codex itself only refreshes an access token within 5 minutes of its expiry,
// lazily on the next request; refreshing well ahead keeps concurrent codex
// processes from racing to rotate the refresh token, and surfaces a broken
// login in the logs before a request fails on it.
func (r *Runner) RunTokenRefresher(ctx context.Context) {
	tr := r.cfg.TokenRefresh
	if !tr.IsEnabled() {
		return
	}
	for {
		if err := r.CheckToken(ctx); err != nil && ctx.Err() == nil {
			if errors.Is(err, ErrNoChatGPTToken) {
				r.logf.Warn("codex token check skipped", "err", err)
			} else {
				r.logf.Error("codex token check", "err", err)
			}
		}
		timer := time.NewTimer(time.Until(tr.At.Next(time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// CheckToken refreshes the ChatGPT login through `codex app-server` when the
// access token in $CODEX_HOME/auth.json expires within token_refresh.threshold,
// and verifies that a later-expiring token was written back.
func (r *Runner) CheckToken(ctx context.Context) error {
	path := filepath.Join(r.cfg.CodexHome, "auth.json")
	exp, err := accessTokenExpiry(path)
	if err != nil {
		return err
	}
	left := time.Until(exp).Round(time.Second)
	threshold := r.cfg.TokenRefresh.Threshold
	if left > threshold {
		r.logf.Info("codex token ok", "expires_at", exp, "left", left, "threshold", threshold)
		return nil
	}
	r.logf.Info("refreshing codex token", "expires_at", exp, "left", left, "threshold", threshold)
	if err := r.refreshToken(ctx); err != nil {
		return err
	}
	// account/read reports success even when the refresh failed, so the
	// written-back token is the only reliable outcome.
	newExp, err := accessTokenExpiry(path)
	if err != nil {
		return err
	}
	if !newExp.After(exp) {
		return fmt.Errorf("codex token not refreshed: still expires at %s (see codex logs under %s)", exp, r.cfg.CodexHome)
	}
	r.logf.Info("codex token refreshed", "expires_at", newExp, "left", time.Until(newExp).Round(time.Second))
	return nil
}

// accessTokenExpiry returns the exp claim of tokens.access_token in auth.json.
func accessTokenExpiry(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, fmt.Errorf("%w: %s does not exist", ErrNoChatGPTToken, path)
	}
	if err != nil {
		return time.Time{}, err
	}
	var auth struct {
		Tokens *struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		return time.Time{}, fmt.Errorf("%w (%s)", ErrNoChatGPTToken, path)
	}
	parts := strings.Split(auth.Tokens.AccessToken, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("parse %s: access_token is not a JWT", path)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: access_token payload: %w", path, err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("parse %s: access_token has no exp claim", path)
	}
	return time.Unix(claims.Exp, 0), nil
}

// refreshToken runs `codex app-server` over stdio and sends account/read with
// refreshToken: true, which forces codex's own refresh flow (it rewrites
// auth.json in place). Closing stdin after the response makes codex exit.
func (r *Runner) refreshToken(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, tokenRefreshTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.cfg.Bin, append([]string{"app-server"}, configOverrides...)...)
	cmd.Env = r.env()
	killGroupOnCancel(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start codex app-server: %w", err)
	}
	defer stdin.Close()

	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"codex-image-api","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"account/read","params":{"refreshToken":true}}`,
	}
	_, writeErr := io.WriteString(stdin, strings.Join(requests, "\n")+"\n")

	// Read until the account/read response, then close stdin and drain stdout
	// so that codex never blocks on a full pipe while exiting.
	var respErr error
	answered := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if answered {
			continue
		}
		var msg struct {
			ID    json.RawMessage `json:"id"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || string(msg.ID) != "2" {
			continue
		}
		answered = true
		if msg.Error != nil {
			respErr = fmt.Errorf("codex app-server account/read: %s", msg.Error.Message)
		}
		stdin.Close()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil && !answered:
		return fmt.Errorf("codex app-server: %w", context.Cause(ctx))
	case respErr != nil:
		return respErr
	case !answered:
		if writeErr == nil {
			writeErr = waitErr
		}
		return fmt.Errorf("codex app-server exited without answering (%v): %s", writeErr, tailBytes(stderr.Bytes(), 1000))
	}
	return nil
}

// tailBytes returns the trimmed last max bytes of b.
func tailBytes(b []byte, max int) string {
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return strings.TrimSpace(string(b))
}
