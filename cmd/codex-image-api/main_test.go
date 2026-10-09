package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/chendefine/codex-image-api/internal/config"
)

// fakeCodex sleeps $FAKE_SLEEP seconds, then saves one image the way the
// built-in image_gen tool does (no rollout, so collection uses mtime order).
const fakeCodex = `#!/bin/sh
cat > /dev/null
echo '{"type":"thread.started","thread_id":"tid-1"}'
sleep "$FAKE_SLEEP"
mkdir -p "$FAKE_HOME/generated_images/tid-1"
cp "$FAKE_PNG" "$FAKE_HOME/generated_images/tid-1/exec-1.png"
`

// startServer runs run() on a random port and returns its base URL, a function
// that triggers shutdown (like SIGTERM) and the channel run's result arrives on.
func startServer(t *testing.T, sleep string, grace time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(dir, "fixture.png")
	if err := os.WriteFile(pngPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "codex_home")
	keep := true
	cfg := &config.Config{
		Server:    config.Server{ShutdownTimeout: grace},
		Workspace: config.Workspace{Dir: filepath.Join(dir, "ws"), Keep: &keep},
		Codex: config.Codex{
			Bin: bin, Sandbox: "workspace-write", CodexHome: home,
			Timeout: time.Minute, MaxConcurrency: 1, MaxQueue: 1, QueueTimeout: time.Minute,
			Env: map[string]string{"FAKE_SLEEP": sleep, "FAKE_HOME": home, "FAKE_PNG": pngPath},
		},
		Limits: config.Limits{MaxImageBytes: 1 << 20, MaxImages: 1, MaxN: 1},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.New(slog.DiscardHandler), ln) }()
	t.Cleanup(cancel)
	return "http://" + ln.Addr().String(), cancel, done
}

// generate sends a generation request and returns the status and error code.
func generate(t *testing.T, base string) (int, string) {
	t.Helper()
	resp, err := http.Post(base+"/v1/images/generations", "application/json", strings.NewReader(`{"prompt":"x"}`))
	if err != nil {
		t.Error(err)
		return 0, ""
	}
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Error.Code
}

func waitHealthy(t *testing.T, base string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			return
		}
	}
	t.Fatal("server did not come up")
}

type result struct {
	status int
	code   string
}

func TestShutdownWaitsForInflight(t *testing.T) {
	base, shutdown, done := startServer(t, "1", 10*time.Second)
	waitHealthy(t, base)
	res := make(chan result, 1)
	go func() { s, c := generate(t, base); res <- result{s, c} }()
	time.Sleep(300 * time.Millisecond)
	shutdown()

	if r := <-res; r.status != http.StatusOK {
		t.Errorf("in-flight request: status %d (%s), want 200", r.status, r.code)
	}
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Error("server still accepts connections after shutdown")
	}
}

func TestShutdownCancelsAfterGrace(t *testing.T) {
	base, shutdown, done := startServer(t, "30", 300*time.Millisecond)
	waitHealthy(t, base)
	res := make(chan result, 1)
	go func() { s, c := generate(t, base); res <- result{s, c} }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	shutdown()

	if r := <-res; r.status != http.StatusServiceUnavailable || r.code != "shutting_down" {
		t.Errorf("in-flight request: status %d (%s), want 503 shutting_down", r.status, r.code)
	}
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("shutdown took %v: codex was not killed", d)
	}
}
