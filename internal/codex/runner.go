// Package codex runs `codex exec` with the imagegen skill and collects the generated images.
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
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chendefine/codex-image-api/internal/config"
	"github.com/chendefine/codex-image-api/internal/logctx"
)

var (
	// ErrTimeout means codex did not finish within codex.timeout.
	ErrTimeout = errors.New("codex exec timed out")
	// ErrNoImage means codex finished but no image file could be found.
	ErrNoImage = errors.New("codex produced no image")
	// ErrBusy means no codex slot became free: the wait queue was full or
	// codex.queue_timeout elapsed.
	ErrBusy = errors.New("all codex slots are busy")
)

// Workdir is the dedicated working directory of one API call.
type Workdir struct {
	ID        string
	Dir       string
	InputDir  string
	OutputDir string
	// ran is set once codex has been started in this directory.
	ran bool
}

// Image is one generated image file.
type Image struct {
	Path        string
	Data        []byte
	ContentType string
}

// RunError wraps a codex failure with diagnostics from the workdir.
type RunError struct {
	Err error
	// LastMessage is the tail of the agent's final message (e.g. a refusal reason).
	LastMessage string
	// Stderr is the tail of codex stderr; internal, for logs only.
	Stderr string
}

func (e *RunError) Error() string {
	msg := e.Err.Error()
	if e.LastMessage != "" {
		msg += "; last message: " + e.LastMessage
	}
	if e.Stderr != "" {
		msg += "; stderr: " + e.Stderr
	}
	return msg
}

func (e *RunError) Unwrap() error { return e.Err }

type Runner struct {
	cfg     config.Codex
	ws      config.Workspace
	sem     chan struct{}
	waiting atomic.Int64
	logf    *slog.Logger
}

func NewRunner(cfg *config.Config, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		cfg:  cfg.Codex,
		ws:   cfg.Workspace,
		sem:  make(chan struct{}, cfg.Codex.MaxConcurrency),
		logf: logger,
	}
}

// NewWorkdir creates <workspace>/<YYYYMMDD>/<id>/ with an output/
// subdirectory. id is the request ID and must be unique. InputDir is only
// created by callers that save input images (edit requests).
func (r *Runner) NewWorkdir(id string) (*Workdir, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("create workdir: invalid id %q", id)
	}
	day := filepath.Join(r.ws.Dir, time.Now().Format("20060102"))
	if err := os.MkdirAll(day, 0o755); err != nil {
		return nil, fmt.Errorf("create workdir: %w", err)
	}
	dir := filepath.Join(day, id)
	// Mkdir (not MkdirAll) so that a reused id never shares a directory.
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create workdir: %w", err)
	}
	wd := &Workdir{
		ID:        id,
		Dir:       dir,
		InputDir:  filepath.Join(dir, "input"),
		OutputDir: filepath.Join(dir, "output"),
	}
	if err := os.Mkdir(wd.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create workdir: %w", err)
	}
	return wd, nil
}

// Cleanup removes the workdir, unless workspace.keep is enabled and codex ran
// in it: requests rejected earlier (e.g. busy) leave nothing worth debugging.
func (r *Runner) Cleanup(ctx context.Context, wd *Workdir) {
	if wd == nil || (r.ws.KeepWorkdirs() && wd.ran) {
		return
	}
	if err := os.RemoveAll(wd.Dir); err != nil {
		r.log(ctx).Warn("remove workdir", "dir", wd.Dir, "err", err)
	}
}

// log returns the request-scoped logger carried by ctx, if any.
func (r *Runner) log(ctx context.Context) *slog.Logger {
	return logctx.From(ctx, r.logf)
}

// acquire takes a codex slot, waiting in a bounded queue for at most
// codex.queue_timeout. Context errors are returned as their cause.
func (r *Runner) acquire(ctx context.Context) (release func(), err error) {
	release = func() { <-r.sem }
	select {
	case r.sem <- struct{}{}:
		return release, nil
	default:
	}
	if r.waiting.Add(1) > int64(r.cfg.MaxQueue) {
		r.waiting.Add(-1)
		return nil, fmt.Errorf("%w: wait queue is full", ErrBusy)
	}
	defer r.waiting.Add(-1)
	timer := time.NewTimer(r.cfg.QueueTimeout)
	defer timer.Stop()
	select {
	case r.sem <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, fmt.Errorf("%w: no slot within %s", ErrBusy, r.cfg.QueueTimeout)
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// BuildArgs returns the `codex exec` arguments. No prompt argument is given, so
// codex reads it from stdin; `-i <FILE>...` is variadic and would swallow any
// positional argument (even "-") as another image, so the -i flags come last.
func (r *Runner) BuildArgs(wd *Workdir, job Job) []string {
	args := []string{"exec", "--skip-git-repo-check", "--json",
		"-C", wd.Dir,
		"-s", r.cfg.Sandbox,
		"-o", filepath.Join(wd.Dir, "last_message.txt"),
	}
	if r.cfg.Model != "" {
		args = append(args, "-m", r.cfg.Model)
	}
	if r.cfg.Profile != "" {
		args = append(args, "-p", r.cfg.Profile)
	}
	args = append(args, r.cfg.ExtraArgs...)
	for _, img := range job.InputImages {
		args = append(args, "-i", img)
	}
	return args
}

// Run executes codex for job inside wd and returns up to job.N images.
// If ctx is canceled, the error is context.Cause(ctx).
func (r *Runner) Run(ctx context.Context, wd *Workdir, job Job) ([]Image, error) {
	release, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	logger := r.log(ctx)
	wd.ran = true

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	prompt := BuildPrompt(job)
	_ = os.WriteFile(filepath.Join(wd.Dir, "prompt.txt"), []byte(prompt), 0o644)

	stdout, err := os.Create(filepath.Join(wd.Dir, "codex.jsonl"))
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(wd.Dir, "codex.stderr.log"))
	if err != nil {
		return nil, err
	}
	defer stderr.Close()

	cmd := exec.CommandContext(runCtx, r.cfg.Bin, r.BuildArgs(wd, job)...)
	cmd.Dir = wd.Dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	for k, v := range r.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Run codex in its own process group so cancellation also kills its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	started := time.Now()
	logger.Info("codex exec start", "dir", wd.Dir, "n", job.N, "edit", job.IsEdit())
	runErr := cmd.Run()
	logger.Info("codex exec done", "elapsed", time.Since(started).Round(time.Millisecond), "err", runErr)

	// The built-in image_gen tool saves every result under
	// $CODEX_HOME/generated_images/<thread_id>/ and the session is recorded in a
	// rollout file; both belong to this run only and are removed after collection.
	tid := ThreadID(filepath.Join(wd.Dir, "codex.jsonl"))
	defer r.cleanupThread(logger, tid)

	if runErr != nil {
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
			return nil, r.runError(wd, ErrTimeout)
		case ctx.Err() != nil:
			return nil, context.Cause(ctx)
		}
	}

	images, collectErr := r.collect(logger, wd, tid, job.N)
	if collectErr != nil {
		return nil, collectErr
	}
	if len(images) == 0 {
		err := ErrNoImage
		if runErr != nil {
			err = fmt.Errorf("%w (codex exec: %v)", ErrNoImage, runErr)
		}
		return nil, r.runError(wd, err)
	}
	return images, nil
}

// collect gathers the images codex generated in thread tid, without relying on
// the agent to copy them anywhere, and stores them as <workdir>/output/image_<i>.<ext>.
//
// The session rollout gives the authoritative list of completed image_gen calls
// in order; each image is read from generated_images/<tid>/<item id>.* or, if the
// file is missing, decoded from the rollout. Without a rollout the
// generated_images directory is read in modification-time order.
// Of these, the images listed in the final message are kept (see selectFinal).
func (r *Runner) collect(logger *slog.Logger, wd *Workdir, tid string, n int) ([]Image, error) {
	if tid == "" || r.cfg.CodexHome == "" {
		return nil, nil
	}
	genDir := filepath.Join(r.cfg.CodexHome, "generated_images", tid)

	var images []Image
	var ids []string
	gens, err := r.rolloutGenerations(tid)
	if err != nil {
		logger.Warn("read codex rollout", "thread", tid, "err", err)
	}
	for _, g := range gens {
		if img, ok := generatedImage(genDir, g); ok {
			images = append(images, img)
			ids = append(ids, g.ID)
		}
	}
	if len(gens) == 0 {
		if images, ids, err = readImages(genDir); err != nil {
			return nil, err
		}
	}
	final := finalImageIDs(filepath.Join(wd.Dir, "last_message.txt"))
	images = selectFinal(images, ids, final, n)

	for i := range images {
		ext := strings.TrimPrefix(images[i].ContentType, "image/")
		if ext == "jpeg" {
			ext = "jpg"
		}
		out := filepath.Join(wd.OutputDir, fmt.Sprintf("image_%d.%s", i+1, ext))
		if err := os.WriteFile(out, images[i].Data, 0o644); err != nil {
			return nil, fmt.Errorf("save output image: %w", err)
		}
		images[i].Path = out
	}
	return images, nil
}

// finalImageLine matches a line of the final message naming a delivered image,
// e.g. "FINAL_IMAGE: `<item id>.png`"; a full path is accepted too.
var finalImageLine = regexp.MustCompile(regexp.QuoteMeta(finalImageMarker) + "\\s*[`\"']?([^\\s`\"']+)")

// finalImageIDs returns the ids (file names without extension) of the images
// the final message lists as delivered, in order.
func finalImageIDs(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ids []string
	for _, m := range finalImageLine.FindAllSubmatch(data, -1) {
		base := filepath.Base(string(m[1]))
		ids = append(ids, strings.TrimSuffix(base, filepath.Ext(base)))
	}
	return ids
}

// selectFinal returns up to n images: those whose id is listed in final, in
// that order, or, if none is listed, the last n, since replaced drafts come
// before the images that replace them.
func selectFinal(images []Image, ids, final []string, n int) []Image {
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		index[id] = i
	}
	var picked []Image
	seen := make(map[int]bool)
	for _, id := range final {
		if i, ok := index[id]; ok && !seen[i] && len(picked) < n {
			seen[i] = true
			picked = append(picked, images[i])
		}
	}
	if len(picked) > 0 {
		return picked
	}
	if len(images) > n {
		return images[len(images)-n:]
	}
	return images
}

// generation is a completed built-in image_gen call recorded in the session rollout.
type generation struct {
	ID     string
	Result string // base64 image
}

// rolloutGenerations returns the completed image_gen calls of thread tid, in order,
// from $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<tid>.jsonl.
func (r *Runner) rolloutGenerations(tid string) ([]generation, error) {
	matches := r.rolloutFiles(tid)
	if len(matches) == 0 {
		return nil, nil
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Lines carrying images are several MB, so read whole lines without a size cap.
	br := bufio.NewReader(f)
	var gens []generation
	for {
		line, readErr := br.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"image_gen.generation"`)) {
			var ev struct {
				Payload struct {
					Type string `json:"type"`
					Item struct {
						Type   string `json:"type"`
						Kind   string `json:"kind"`
						ID     string `json:"id"`
						Status string `json:"status"`
						Result string `json:"result"`
					} `json:"item"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &ev) == nil && ev.Payload.Type == "item_completed" {
				it := ev.Payload.Item
				if it.Kind == "image_gen.generation" && it.Status == "completed" && it.ID != "" {
					gens = append(gens, generation{ID: it.ID, Result: it.Result})
				}
			}
		}
		if readErr == io.EOF {
			return gens, nil
		}
		if readErr != nil {
			return gens, readErr
		}
	}
}

func generatedImage(genDir string, g generation) (Image, bool) {
	if validID.MatchString(g.ID) {
		if matches, _ := filepath.Glob(filepath.Join(genDir, g.ID+".*")); len(matches) > 0 {
			if data, err := os.ReadFile(matches[0]); err == nil {
				if img, ok := toImage(data); ok {
					return img, true
				}
			}
		}
	}
	data, err := base64.StdEncoding.DecodeString(g.Result)
	if err != nil {
		return Image{}, false
	}
	return toImage(data)
}

func toImage(data []byte) (Image, bool) {
	switch ct := http.DetectContentType(data); ct {
	case "image/png", "image/jpeg", "image/webp":
		return Image{Data: data, ContentType: ct}, true
	}
	return Image{}, false
}

// validID guards paths built from ids read from codex output.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// cleanupThread deletes what codex stored for thread tid under $CODEX_HOME:
// generated_images/<tid>/ and the session rollout file(s).
func (r *Runner) cleanupThread(logger *slog.Logger, tid string) {
	if tid == "" || r.cfg.CodexHome == "" || !validID.MatchString(tid) {
		return
	}
	dir := filepath.Join(r.cfg.CodexHome, "generated_images", tid)
	if err := os.RemoveAll(dir); err != nil {
		logger.Warn("remove codex generated images", "dir", dir, "err", err)
	}
	for _, f := range r.rolloutFiles(tid) {
		if err := os.Remove(f); err != nil {
			logger.Warn("remove codex rollout", "file", f, "err", err)
		}
	}
}

func (r *Runner) rolloutFiles(tid string) []string {
	matches, _ := filepath.Glob(filepath.Join(r.cfg.CodexHome, "sessions", "*", "*", "*", "rollout-*-"+tid+".jsonl"))
	return matches
}

// readImages returns the images in dir in modification-time order, with their
// ids (file names without extension).
func readImages(dir string) ([]Image, []string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	type item struct {
		path  string
		mtime time.Time
	}
	var items []item
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].mtime.Equal(items[j].mtime) {
			return items[i].mtime.Before(items[j].mtime)
		}
		return items[i].path < items[j].path
	})

	var images []Image
	var ids []string
	for _, it := range items {
		data, err := os.ReadFile(it.path)
		if err != nil {
			return nil, nil, err
		}
		if img, ok := toImage(data); ok {
			base := filepath.Base(it.path)
			images = append(images, img)
			ids = append(ids, strings.TrimSuffix(base, filepath.Ext(base)))
		}
	}
	return images, ids, nil
}

// ThreadID extracts the thread id from the `thread.started` event of a codex --json log.
func ThreadID(jsonlPath string) string {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if !bytes.Contains(line, []byte("thread.started")) {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "thread.started" && ev.ThreadID != "" {
			return ev.ThreadID
		}
	}
	return ""
}

func (r *Runner) runError(wd *Workdir, err error) *RunError {
	return &RunError{
		Err:         err,
		LastMessage: tail(filepath.Join(wd.Dir, "last_message.txt"), 1000),
		Stderr:      tail(filepath.Join(wd.Dir, "codex.stderr.log"), 1000),
	}
}

func tail(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > max {
		_, _ = f.Seek(-max, io.SeekEnd)
	}
	data, _ := io.ReadAll(f)
	return strings.TrimSpace(string(data))
}
