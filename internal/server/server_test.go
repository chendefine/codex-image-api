package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/chendefine/codex-image-api/internal/codex"
	"github.com/chendefine/codex-image-api/internal/config"
)

// fakeCodex mimics `codex exec` with the built-in image_gen tool: it records
// args/stdin in the workdir and saves images where codex does, under
// $CODEX_HOME/generated_images/<thread_id>/ plus a session rollout, never in the workdir.
const fakeCodex = `#!/bin/sh
dir=""; prev=""
for a in "$@"; do
  if [ "$prev" = "-C" ]; then dir="$a"; fi
  prev="$a"
done
printf '%s\n' "$@" > "$dir/args.txt"
cat > "$dir/stdin.txt"
echo '{"type":"thread.started","thread_id":"'"$FAKE_TID"'"}'
gen="$FAKE_HOME/generated_images/$FAKE_TID"
sess="$FAKE_HOME/sessions/2026/10/09"
generate() {
  mkdir -p "$gen" "$sess"
  i=1
  while [ $i -le "$FAKE_COUNT" ]; do
    cp "$FAKE_PNG" "$gen/exec-$i.png"
    if [ "$1" = rollout ]; then
      printf '{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Extension","kind":"image_gen.generation","id":"exec-%s","status":"completed","result":"%s"}}}\n' \
        "$i" "$(base64 -w0 "$FAKE_PNG")" >> "$sess/rollout-2026-10-09T00-00-00-$FAKE_TID.jsonl"
    fi
    i=$((i+1))
  done
}
case "$FAKE_MODE" in
  generate) generate rollout ;;
  norollout) generate ;;
  sleep)
    mkdir -p "$gen"; cp "$FAKE_PNG" "$gen/exec-partial.png"
    sleep 30 ;;
  fail)
    echo "I cannot help with that request." > "$dir/last_message.txt"
    echo boom >&2
    exit 1 ;;
esac
`

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

type testEnv struct {
	handler http.Handler
	cfg     *config.Config
	png     []byte
	logs    *syncBuffer
}

// syncBuffer collects log output from concurrent goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newEnv(t *testing.T, mode string, count int, mutate func(*config.Config)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	pngData := pngBytes(t, 64, 48)
	pngPath := filepath.Join(dir, "fixture.png")
	if err := os.WriteFile(pngPath, pngData, 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "codex_home")
	cfg := &config.Config{
		Workspace: config.Workspace{Dir: filepath.Join(dir, "ws"), Keep: config.KeepForever()},
		Codex: config.Codex{
			Bin: bin, Sandbox: "workspace-write", CodexHome: home,
			Timeout: 10 * time.Second, MaxConcurrency: 2, MaxQueue: 16, QueueTimeout: time.Minute,
			Env: map[string]string{
				"FAKE_MODE": mode, "FAKE_COUNT": string(rune('0' + count)),
				"FAKE_PNG": pngPath, "FAKE_HOME": home, "FAKE_TID": "thread-123",
			},
		},
		Limits: config.Limits{MaxImageBytes: 1 << 20, MaxImages: 5, MaxN: 10},
	}
	if mutate != nil {
		mutate(cfg)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	runner := codex.NewRunner(cfg, logger)
	return &testEnv{handler: New(cfg, runner, logger).Handler(), cfg: cfg, png: pngData, logs: logs}
}

func (e *testEnv) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return w
}

func jsonReq(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeImages(t *testing.T, w *httptest.ResponseRecorder) ImagesResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var rsp ImagesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rsp); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return rsp
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder, status int) errorDetail {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d, body = %s", w.Code, status, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Message == "" {
		t.Fatalf("not an OpenAI error body: %s", w.Body.String())
	}
	return body.Error
}

// onlyWorkdir returns the single per-request directory created under the workspace.
func onlyWorkdir(t *testing.T, cfg *config.Config) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(cfg.Workspace.Dir, "*", "*"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 workdir, got %v", matches)
	}
	return matches[0]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGenerations(t *testing.T) {
	env := newEnv(t, "generate", 2, nil)
	w := env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{
		"model": "gpt-image-2", "prompt": "a red apple", "n": 2, "size": "1024x1024",
		"quality": "high", "background": "transparent", "output_format": "webp",
	}))
	rsp := decodeImages(t, w)
	if len(rsp.Data) != 2 || rsp.Created == 0 {
		t.Fatalf("rsp = %+v", rsp)
	}
	got, _ := base64.StdEncoding.DecodeString(rsp.Data[0].B64JSON)
	if !bytes.Equal(got, env.png) {
		t.Errorf("image bytes mismatch")
	}
	if rsp.Size != "64x48" || rsp.OutputFormat != "png" || rsp.Background != "transparent" {
		t.Errorf("metadata = %+v", rsp)
	}

	wd := onlyWorkdir(t, env.cfg)
	if id := w.Header().Get("X-Request-Id"); filepath.Base(wd) != id {
		t.Errorf("workdir %s is not named after X-Request-Id %q", wd, id)
	}
	stdin := readFile(t, filepath.Join(wd, "stdin.txt"))
	for _, want := range []string{"$imagegen", "Deliver exactly 2 final images", "transparent_background=true", "a red apple", "1:1 (width:height) aspect ratio, square"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("stdin prompt missing %q", want)
		}
	}
	if strings.Contains(stdin, wd) {
		t.Errorf("prompt must not reveal the workdir/output path:\n%s", stdin)
	}
	args := readFile(t, filepath.Join(wd, "args.txt"))
	if strings.Contains(args, "\n-i\n") || strings.Contains(args, "\n-\n") {
		t.Errorf("unexpected args:\n%s", args)
	}
	for i := 1; i <= 2; i++ {
		if data, err := os.ReadFile(filepath.Join(wd, "output", fmt.Sprintf("image_%d.png", i))); err != nil || !bytes.Equal(data, env.png) {
			t.Errorf("output/image_%d.png not stored: %v", i, err)
		}
	}
	assertGeneratedRemoved(t, env)
}

func assertGeneratedRemoved(t *testing.T, env *testEnv) {
	t.Helper()
	dir := filepath.Join(env.cfg.Codex.CodexHome, "generated_images", "thread-123")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s not removed: %v", dir, err)
	}
	rollouts, _ := filepath.Glob(filepath.Join(env.cfg.Codex.CodexHome, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	if len(rollouts) != 0 {
		t.Errorf("rollout not removed: %v", rollouts)
	}
}

func TestEditsMultipart(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("prompt", "make it green")
	_ = mw.WriteField("size", "1536x1024")
	for _, name := range []string{"a.png", "b.png"} {
		fw, _ := mw.CreateFormFile("image[]", name)
		_, _ = fw.Write(env.png)
	}
	fw, _ := mw.CreateFormFile("mask", "mask.png")
	_, _ = fw.Write(env.png)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	rsp := decodeImages(t, env.do(t, req))
	if len(rsp.Data) != 1 || rsp.Background != "opaque" {
		t.Fatalf("rsp = %+v", rsp)
	}
	wd := onlyWorkdir(t, env.cfg)
	in1, in2 := filepath.Join(wd, "input", "image_1.png"), filepath.Join(wd, "input", "image_2.png")
	args := readFile(t, filepath.Join(wd, "args.txt"))
	if !strings.HasSuffix(args, "-i\n"+in1+"\n-i\n"+in2+"\n") {
		t.Errorf("input images not passed via -i:\n%s", args)
	}
	stdin := readFile(t, filepath.Join(wd, "stdin.txt"))
	if !strings.Contains(stdin, "[Image #1] ... [Image #2]") || !strings.Contains(stdin, "edit the input image(s)") ||
		!strings.Contains(stdin, "3:2 (width:height) aspect ratio, landscape") {
		t.Errorf("edit prompt wrong:\n%s", stdin)
	}
	if strings.Contains(stdin, wd) {
		t.Errorf("edit prompt must not contain paths:\n%s", stdin)
	}
	assertGeneratedRemoved(t, env)
}

func TestEditsSingleImageField(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("prompt", "add a hat")
	fw, _ := mw.CreateFormFile("image", "a.png")
	_, _ = fw.Write(env.png)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	decodeImages(t, env.do(t, req))
}

func TestEditsJSONDataURL(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(env.png)
	w := env.do(t, jsonReq(t, "/v1/images/edits", map[string]any{
		"prompt": "make it blue", "images": []map[string]string{{"image_url": dataURL}},
	}))
	decodeImages(t, w)
	wd := onlyWorkdir(t, env.cfg)
	if _, err := os.Stat(filepath.Join(wd, "input", "image_1.png")); err != nil {
		t.Errorf("input not saved: %v", err)
	}
	if stdin := readFile(t, filepath.Join(wd, "stdin.txt")); strings.Contains(stdin, "aspect ratio") {
		t.Errorf("prompt must not mention an aspect ratio without size:\n%s", stdin)
	}
}

func TestEditsRejections(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	cases := map[string]map[string]any{
		"file_id":    {"prompt": "x", "images": []map[string]string{{"file_id": "file-1"}}},
		"remote url": {"prompt": "x", "images": []map[string]string{{"image_url": "https://example.com/a.png"}}},
		"no images":  {"prompt": "x"},
		"not image":  {"prompt": "x", "images": []map[string]string{{"image_url": "data:text/plain;base64,aGVsbG8="}}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			decodeError(t, env.do(t, jsonReq(t, "/v1/images/edits", body)), http.StatusBadRequest)
		})
	}
}

func TestGenerationsValidation(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	cases := map[string]struct {
		body  any
		param string
	}{
		"missing prompt": {map[string]any{"n": 1}, "prompt"},
		"n too large":    {map[string]any{"prompt": "x", "n": 11}, "n"},
		"bad background": {map[string]any{"prompt": "x", "background": "green"}, "background"},
		"bad size":       {map[string]any{"prompt": "x", "size": "large"}, "size"},
		"zero size":      {map[string]any{"prompt": "x", "size": "0x512"}, "size"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := decodeError(t, env.do(t, jsonReq(t, "/v1/images/generations", tc.body)), http.StatusBadRequest)
			if e.Param == nil || *e.Param != tc.param || e.Type != "invalid_request_error" {
				t.Errorf("error = %+v", e)
			}
		})
	}

	for _, body := range []string{"{bad", "", `{"prompt":"x"}}`, `{"prompt":"x"} {}`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		decodeError(t, env.do(t, req), http.StatusBadRequest)
	}
	big := `{"prompt":"` + strings.Repeat("x", maxJSONBody) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	if e := decodeError(t, env.do(t, req), http.StatusBadRequest); !strings.Contains(e.Message, "too large") {
		t.Errorf("message = %q", e.Message)
	}
}

func TestAuth(t *testing.T) {
	env := newEnv(t, "generate", 1, func(c *config.Config) { c.Server.APIKeys = []string{"sk-other", "sk-test"} })
	for _, h := range []string{"", "Bearer sk-wrong", "Bearer sk-tes", "Basic sk-test", "sk-test"} {
		req := jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"})
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		w := env.do(t, req)
		e := decodeError(t, w, http.StatusUnauthorized)
		if e.Code == nil || *e.Code != "invalid_api_key" || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("Authorization %q: error = %+v, headers = %v", h, e, w.Header())
		}
	}
	for _, h := range []string{"Bearer sk-test", "bearer sk-other"} {
		req := jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"})
		req.Header.Set("Authorization", h)
		decodeImages(t, env.do(t, req))
	}
}

func TestWithoutRollout(t *testing.T) {
	env := newEnv(t, "norollout", 2, nil)
	rsp := decodeImages(t, env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x", "n": 2})))
	if len(rsp.Data) != 2 {
		t.Fatalf("rsp = %+v", rsp)
	}
	assertGeneratedRemoved(t, env)
}

func TestCodexFailure(t *testing.T) {
	env := newEnv(t, "fail", 1, nil)
	e := decodeError(t, env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"})), http.StatusInternalServerError)
	if !strings.Contains(e.Message, "I cannot help") || strings.Contains(e.Message, "boom") {
		t.Errorf("message = %q", e.Message)
	}
}

func TestTimeout(t *testing.T) {
	env := newEnv(t, "sleep", 1, func(c *config.Config) { c.Codex.Timeout = 300 * time.Millisecond })
	start := time.Now()
	decodeError(t, env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"})), http.StatusGatewayTimeout)
	if time.Since(start) > 8*time.Second {
		t.Errorf("timeout did not kill codex promptly: %v", time.Since(start))
	}
	assertGeneratedRemoved(t, env)
}

func TestCleanupWhenKeepDisabled(t *testing.T) {
	env := newEnv(t, "generate", 1, func(c *config.Config) { c.Workspace.Keep = config.KeepDays(0) })
	decodeImages(t, env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"})))
	if matches, _ := filepath.Glob(filepath.Join(env.cfg.Workspace.Dir, "*", "*")); len(matches) != 0 {
		t.Errorf("workdir not removed: %v", matches)
	}
}

func TestKeepDaysKeepsWorkdir(t *testing.T) {
	env := newEnv(t, "generate", 1, func(c *config.Config) { c.Workspace.Keep = config.KeepDays(1) })
	decodeImages(t, env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"})))
	if matches, _ := filepath.Glob(filepath.Join(env.cfg.Workspace.Dir, "*", "*")); len(matches) != 1 {
		t.Errorf("workdirs = %v, want the request's one", matches)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	e := decodeError(t, env.do(t, httptest.NewRequest(http.MethodGet, "/v1/nope", nil)), http.StatusNotFound)
	if e.Type != "invalid_request_error" {
		t.Errorf("error = %+v", e)
	}
	decodeError(t, env.do(t, httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)), http.StatusMethodNotAllowed)
}

func TestRequestIDHeader(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	id := env.do(t, httptest.NewRequest(http.MethodGet, "/healthz", nil)).Header().Get("X-Request-Id")
	if !requestIDPattern.MatchString(id) {
		t.Fatalf("X-Request-Id = %q, want 16 lowercase hex digits", id)
	}
	// The first 11 digits are the Unix time in milliseconds.
	ms, _ := strconv.ParseInt(id[:11], 16, 64)
	if d := time.Since(time.UnixMilli(ms)); d < 0 || d > time.Minute {
		t.Errorf("X-Request-Id %q encodes %v, not the current time", id, time.UnixMilli(ms))
	}
}

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestNewRequestIDIncreasing(t *testing.T) {
	// Far more IDs per millisecond than real traffic, from concurrent goroutines.
	const workers, perWorker = 8, 5000
	ids := make(chan string, workers*perWorker)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			prev := ""
			for range perWorker {
				id := newRequestID()
				if id <= prev {
					t.Errorf("IDs not increasing within a goroutine: %q after %q", id, prev)
					return
				}
				prev = id
				ids <- id
			}
		})
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool, workers*perWorker)
	for id := range ids {
		if !requestIDPattern.MatchString(id) || seen[id] {
			t.Fatalf("bad or duplicate ID %q", id)
		}
		seen[id] = true
	}
}

func TestPanicRecovery(t *testing.T) {
	s := New(&config.Config{}, nil, nil)
	r := gin.New()
	r.Use(s.requestID, s.recovery)
	r.GET("/panic", handle(s, func(*gin.Context) (*ImagesResponse, error) { panic("kaboom") }))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil))
	e := decodeError(t, w, http.StatusInternalServerError)
	if strings.Contains(e.Message, "kaboom") {
		t.Errorf("panic value leaked to client: %q", e.Message)
	}
}

func TestClientCancel(t *testing.T) {
	env := newEnv(t, "sleep", 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	req := jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"}).WithContext(ctx)
	start := time.Now()
	decodeError(t, env.do(t, req), 499)
	if time.Since(start) > 8*time.Second {
		t.Errorf("cancel did not kill codex promptly: %v", time.Since(start))
	}
	assertGeneratedRemoved(t, env)
}

func TestShutdownCancel(t *testing.T) {
	env := newEnv(t, "sleep", 1, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(300*time.Millisecond, func() { cancel(ErrShuttingDown) })
	req := jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"}).WithContext(ctx)
	w := env.do(t, req)
	e := decodeError(t, w, http.StatusServiceUnavailable)
	if e.Code == nil || *e.Code != "shutting_down" || w.Header().Get("Retry-After") == "" {
		t.Errorf("error = %+v", e)
	}
	assertGeneratedRemoved(t, env)
}

func TestRequestLogsCorrelated(t *testing.T) {
	env := newEnv(t, "generate", 1, nil)
	req := jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"})
	req.Header.Set("X-Client-Request-Id", "client-42\nforged=1")
	w := env.do(t, req)
	decodeImages(t, w)
	id := w.Header().Get("X-Request-Id")
	for _, msg := range []string{"codex exec start", "codex exec done", `msg=request`} {
		found := false
		for _, line := range strings.Split(env.logs.String(), "\n") {
			if strings.Contains(line, msg) {
				found = true
				if !strings.Contains(line, "request_id="+id) || !strings.Contains(line, `client_request_id="client-42forged=1"`) {
					t.Errorf("log line not correlated: %s", line)
				}
			}
		}
		if !found {
			t.Errorf("no %q log line:\n%s", msg, env.logs.String())
		}
	}
}

func TestBusy(t *testing.T) {
	env := newEnv(t, "sleep", 1, func(c *config.Config) {
		c.Codex.MaxConcurrency, c.Codex.MaxQueue, c.Codex.QueueTimeout = 1, 1, 300*time.Millisecond
	})
	// Occupy the only slot with a long-running generation.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"}).WithContext(ctx))
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m, _ := filepath.Glob(filepath.Join(env.cfg.Workspace.Dir, "*", "*", "stdin.txt")); len(m) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first request never started codex")
		}
		time.Sleep(10 * time.Millisecond)
	}

	w := env.do(t, jsonReq(t, "/v1/images/generations", map[string]any{"prompt": "x"}))
	e := decodeError(t, w, http.StatusServiceUnavailable)
	if e.Code == nil || *e.Code != "server_busy" || w.Header().Get("Retry-After") == "" {
		t.Errorf("error = %+v, Retry-After = %q", e, w.Header().Get("Retry-After"))
	}
	// Only the running request keeps its workdir.
	onlyWorkdir(t, env.cfg)
}
