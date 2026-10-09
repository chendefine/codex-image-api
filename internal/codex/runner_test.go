package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chendefine/codex-image-api/internal/config"
)

func TestBuildPromptGenerate(t *testing.T) {
	p := BuildPrompt(Job{Prompt: "  a red apple  ", N: 1})
	if !strings.HasPrefix(p, "$imagegen\n") {
		t.Fatalf("prompt must start with the $imagegen skill mention:\n%s", p)
	}
	for _, want := range []string{
		"generate a brand-new image",
		"exactly 1 separate built-in image_gen call ",
		"Do not create or modify any other files.",
		"<<<\na red apple\n>>>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "output") || strings.Contains(p, "image_1.png") {
		t.Errorf("prompt must not name an output location:\n%s", p)
	}
	if strings.Contains(p, "transparent_background") {
		t.Errorf("non-transparent prompt must not mention transparent_background:\n%s", p)
	}
	if strings.Contains(p, "referenced_image_paths, in this order") {
		t.Errorf("generate prompt must not list input images")
	}
}

func TestBuildPromptEdit(t *testing.T) {
	job := Job{Prompt: "make it green", N: 3, Transparent: true, InputImages: []string{"/a/1.png", "/a/2.jpg"}}
	p := BuildPrompt(job)
	for _, want := range []string{
		"edit the input image(s)",
		"exactly 3 separate built-in image_gen calls",
		"transparent_background=true",
		"[Image #1] ... [Image #2]",
		"referenced_image_paths",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "/a/") {
		t.Errorf("prompt must not contain input image paths:\n%s", p)
	}
}

func TestBuildPromptAspectRatio(t *testing.T) {
	if p := BuildPrompt(Job{Prompt: "x", N: 1}); strings.Contains(p, "aspect ratio") {
		t.Errorf("no size: prompt must not mention an aspect ratio:\n%s", p)
	}
	p := BuildPrompt(Job{Prompt: "x", N: 1, AspectRatio: "9:16"})
	if !strings.Contains(p, "9:16 (width:height) aspect ratio, portrait") {
		t.Errorf("prompt missing aspect ratio:\n%s", p)
	}
}

func TestAspectRatio(t *testing.T) {
	cases := []struct {
		w, h int
		want string
	}{
		{1024, 1024, "1:1"},
		{1536, 1024, "3:2"},
		{1024, 1536, "2:3"},
		{1920, 1080, "16:9"},
		{1080, 1920, "9:16"},
		{1280, 800, "16:10"},
		{1024, 768, "4:3"},
		{1280, 1024, "5:4"},
		{2560, 1080, "21:9"}, // 2.37 vs 2.33
		{3440, 1440, "21:9"},
		{2048, 1024, "2:1"},
		{1000, 980, "1:1"},  // within 5%
		{1430, 1000, "3:2"}, // 4.7% from 3:2, 7.3% from 4:3
		{1000, 400, "5:2"},  // 2.5 is 7% from 21:9: not common
		{1000, 300, "3:1"},  // 3.33 clamped to the 3:1 maximum
		{300, 1000, "1:3"},  // 0.3 clamped to the 1:3 minimum
		{4000, 500, "3:1"},
		{512, 4096, "1:3"},
		{2800, 1000, "14:5"}, // 2.8: inside the range, kept
		{2100, 1000, "21:10"},
		{1234, 567, "24:11"}, // closest ratio with terms <= 25
		{3000, 1000, "3:1"},
	}
	for _, c := range cases {
		if got := AspectRatio(c.w, c.h); got != c.want {
			t.Errorf("AspectRatio(%d, %d) = %s, want %s", c.w, c.h, got, c.want)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	cfg := &config.Config{Codex: config.Codex{
		Sandbox: "workspace-write", Model: "gpt-x", Profile: "img", MaxConcurrency: 1,
		ExtraArgs: []string{"-c", "a=1"},
	}}
	r := NewRunner(cfg, nil)
	wd := &Workdir{Dir: "/ws/d"}
	args := r.BuildArgs(wd, Job{InputImages: []string{"/ws/d/input/image_1.png", "/ws/d/input/image_2.png"}})
	want := []string{"exec", "--skip-git-repo-check", "--json", "-C", "/ws/d", "-s", "workspace-write",
		"-o", "/ws/d/last_message.txt", "-m", "gpt-x", "-p", "img", "-c", "a=1",
		"-i", "/ws/d/input/image_1.png", "-i", "/ws/d/input/image_2.png"}
	if !slices.Equal(args, want) {
		t.Fatalf("args mismatch\n got: %q\nwant: %q", args, want)
	}
}

func TestThreadID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "codex.jsonl")
	data := `{"type":"turn.started"}
not json
{"type":"thread.started","thread_id":"019f-abc"}
`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ThreadID(p); got != "019f-abc" {
		t.Fatalf("ThreadID = %q", got)
	}
	if got := ThreadID(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("ThreadID(missing) = %q", got)
	}
}

func pngData(t *testing.T, w int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func rolloutLine(id, status string, result []byte) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Extension","kind":"image_gen.generation","id":%q,"status":%q,"result":%q}}}`+"\n",
		id, status, base64.StdEncoding.EncodeToString(result))
}

func TestCollectFromRollout(t *testing.T) {
	home := t.TempDir()
	tid := "01a1-thread"
	genDir := filepath.Join(home, "generated_images", tid)
	sessDir := filepath.Join(home, "sessions", "2026", "10", "09")
	for _, d := range []string{genDir, sessDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	imgA, imgB, stray := pngData(t, 1), pngData(t, 2), pngData(t, 3)
	// exec-b has no file: it must be decoded from the rollout; exec-x failed; stray is not in the rollout.
	_ = os.WriteFile(filepath.Join(genDir, "exec-a.png"), imgA, 0o644)
	_ = os.WriteFile(filepath.Join(genDir, "stray.png"), stray, 0o644)
	rollout := `{"type":"session_meta","payload":{}}` + "\n" +
		rolloutLine("exec-a", "completed", []byte("ignored when file exists")) +
		rolloutLine("exec-x", "failed", nil) +
		rolloutLine("exec-b", "completed", imgB)
	_ = os.WriteFile(filepath.Join(sessDir, "rollout-2026-10-09T16-39-04-"+tid+".jsonl"), []byte(rollout), 0o644)

	cfg := &config.Config{
		Workspace: config.Workspace{Dir: t.TempDir()},
		Codex:     config.Codex{CodexHome: home, MaxConcurrency: 1},
	}
	r := NewRunner(cfg, nil)
	wd, err := r.NewWorkdir("collect")
	if err != nil {
		t.Fatal(err)
	}
	images, err := r.collect(r.logf, wd, tid, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || !bytes.Equal(images[0].Data, imgA) || !bytes.Equal(images[1].Data, imgB) {
		t.Fatalf("got %d images, want [exec-a, exec-b]", len(images))
	}
	for i, img := range images {
		want := filepath.Join(wd.OutputDir, fmt.Sprintf("image_%d.png", i+1))
		if data, err := os.ReadFile(want); img.Path != want || err != nil || !bytes.Equal(data, img.Data) {
			t.Errorf("image %d not stored at %s (path %s, err %v)", i+1, want, img.Path, err)
		}
	}

	r.cleanupThread(r.logf, tid)
	if _, err := os.Stat(genDir); !os.IsNotExist(err) {
		t.Errorf("generated_images/%s not removed: %v", tid, err)
	}
	if left := r.rolloutFiles(tid); len(left) != 0 {
		t.Errorf("rollout not removed: %v", left)
	}
}

func TestCollectWithoutRollout(t *testing.T) {
	home := t.TempDir()
	genDir := filepath.Join(home, "generated_images", "tid")
	_ = os.MkdirAll(genDir, 0o755)
	_ = os.WriteFile(filepath.Join(genDir, "exec-1.png"), pngData(t, 1), 0o644)
	_ = os.WriteFile(filepath.Join(genDir, "exec-2.png"), pngData(t, 2), 0o644)
	cfg := &config.Config{Workspace: config.Workspace{Dir: t.TempDir()}, Codex: config.Codex{CodexHome: home, MaxConcurrency: 1}}
	r := NewRunner(cfg, nil)
	wd, _ := r.NewWorkdir("norollout")
	images, err := r.collect(r.logf, wd, "tid", 1)
	if err != nil || len(images) != 1 {
		t.Fatalf("images = %d, err = %v", len(images), err)
	}
}

func TestNewWorkdir(t *testing.T) {
	cfg := &config.Config{Workspace: config.Workspace{Dir: t.TempDir()}, Codex: config.Codex{MaxConcurrency: 1}}
	r := NewRunner(cfg, nil)
	wd, err := r.NewWorkdir("1a120aaf66a00b7c")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg.Workspace.Dir, time.Now().Format("20060102"), "1a120aaf66a00b7c"); wd.Dir != want || wd.ID != "1a120aaf66a00b7c" {
		t.Errorf("workdir = %+v, want dir %s", wd, want)
	}
	if _, err := r.NewWorkdir("1a120aaf66a00b7c"); err == nil {
		t.Error("reused id must not share a workdir")
	}
	for _, id := range []string{"", "..", "../x", "a/b"} {
		if _, err := r.NewWorkdir(id); err == nil {
			t.Errorf("NewWorkdir(%q) accepted an unsafe id", id)
		}
	}
}

func TestAcquireQueue(t *testing.T) {
	cfg := &config.Config{Codex: config.Codex{MaxConcurrency: 1, MaxQueue: 1, QueueTimeout: 200 * time.Millisecond}}
	r := NewRunner(cfg, nil)
	release, err := r.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// One waiter fits in the queue and times out; a second one is rejected at once.
	waitErr := make(chan error, 1)
	go func() { _, err := r.acquire(context.Background()); waitErr <- err }()
	for r.waiting.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if _, err := r.acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("queue full: err = %v, want ErrBusy", err)
	}
	if err := <-waitErr; !errors.Is(err, ErrBusy) {
		t.Errorf("queue timeout: err = %v, want ErrBusy", err)
	}

	// Cancellation returns the cause.
	cause := errors.New("bye")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	if _, err := r.acquire(ctx); err != cause {
		t.Errorf("canceled: err = %v, want cause", err)
	}

	// A released slot is handed to the next caller.
	release()
	if release, err = r.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	release()
}
