package codex

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chendefine/codex-image-api/internal/config"
)

// fakeAppServer answers the account/read request like `codex app-server`: it
// writes $NEW_AUTH (if set) over $CODEX_HOME/auth.json, replies with
// $RESPONSE, records its arguments and exits once stdin is closed.
const fakeAppServer = `#!/bin/sh
echo "$@" > "$CODEX_HOME/args"
while IFS= read -r line; do
  case "$line" in
    *'"id":1'*) echo '{"jsonrpc":"2.0","id":1,"result":{}}' ;;
    *account/read*)
      [ -n "$NEW_AUTH" ] && printf '%s' "$NEW_AUTH" > "$CODEX_HOME/auth.json"
      echo '{"jsonrpc":"2.0","method":"account/updated","params":{}}'
      echo "$RESPONSE" ;;
  esac
done
echo exited > "$CODEX_HOME/exited"
`

func authJSON(exp time.Time) string {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":"h.%s.s","refresh_token":"r"},"last_refresh":"x"}`, claims)
}

func tokenRunner(t *testing.T, auth, newAuth, response string) (*Runner, string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "codex")
	if err := os.WriteFile(bin, []byte(fakeAppServer), 0o755); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(auth), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if response == "" {
		response = `{"jsonrpc":"2.0","id":2,"result":{"account":null}}`
	}
	cfg := &config.Config{Codex: config.Codex{
		Bin: bin, CodexHome: home, MaxConcurrency: 1,
		Env:          map[string]string{"CODEX_HOME": home, "NEW_AUTH": newAuth, "RESPONSE": response},
		TokenRefresh: config.TokenRefresh{Threshold: 25 * time.Hour},
	}}
	return NewRunner(cfg, nil), home
}

func TestCheckTokenFresh(t *testing.T) {
	r, home := tokenRunner(t, authJSON(time.Now().Add(26*time.Hour)), "", "")
	if err := r.CheckToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "args")); err == nil {
		t.Error("codex app-server ran although the token is far from expiry")
	}
}

func TestCheckTokenRefresh(t *testing.T) {
	newExp := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	r, home := tokenRunner(t, authJSON(time.Now().Add(24*time.Hour)), authJSON(newExp), "")
	if err := r.CheckToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := accessTokenExpiry(filepath.Join(home, "auth.json")); err != nil || !got.Equal(newExp) {
		t.Errorf("expiry after refresh = %s, %v; want %s", got, err, newExp)
	}
	args, _ := os.ReadFile(filepath.Join(home, "args"))
	if !strings.HasPrefix(string(args), "app-server -c project_doc_max_bytes=0") {
		t.Errorf("args = %q", args)
	}
	if _, err := os.Stat(filepath.Join(home, "exited")); err != nil {
		t.Error("codex app-server did not exit on stdin EOF")
	}
}

func TestCheckTokenNotRefreshed(t *testing.T) {
	// account/read succeeds even when codex failed to refresh.
	r, _ := tokenRunner(t, authJSON(time.Now().Add(time.Hour)), "", "")
	if err := r.CheckToken(context.Background()); err == nil || !strings.Contains(err.Error(), "not refreshed") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckTokenRPCError(t *testing.T) {
	r, _ := tokenRunner(t, authJSON(time.Now().Add(time.Hour)), "", `{"jsonrpc":"2.0","id":2,"error":{"code":-1,"message":"boom"}}`)
	if err := r.CheckToken(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckTokenNoChatGPT(t *testing.T) {
	for _, auth := range []string{"", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`} {
		r, _ := tokenRunner(t, auth, "", "")
		if err := r.CheckToken(context.Background()); !errors.Is(err, ErrNoChatGPTToken) {
			t.Errorf("auth %q: err = %v", auth, err)
		}
	}
}
