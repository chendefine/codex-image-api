// Command codex-image-api serves OpenAI-compatible image endpoints backed by `codex exec`.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/chendefine/codex-image-api/internal/codex"
	"github.com/chendefine/codex-image-api/internal/config"
	"github.com/chendefine/codex-image-api/internal/server"
)

// cleanupGrace is how long canceled requests get to kill codex and remove
// their temporary files once the shutdown grace period has elapsed.
const cleanupGrace = 10 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	gin.SetMode(gin.ReleaseMode)

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		logger.Error("listen", "addr", cfg.Server.Listen, "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore default signal handling after the first signal, so that a second
	// Ctrl+C terminates at once instead of waiting for the graceful shutdown.
	context.AfterFunc(ctx, stop)
	if err := run(ctx, cfg, logger, ln); err != nil {
		logger.Error("exit", "err", err)
		os.Exit(1)
	}
}

// run serves on ln until ctx is canceled, then shuts down gracefully: new
// connections are refused, in-flight requests get server.shutdown_timeout to
// finish and are canceled afterwards (killing their codex processes).
func run(ctx context.Context, cfg *config.Config, logger *slog.Logger, ln net.Listener) error {
	if err := os.MkdirAll(cfg.Workspace.Dir, 0o755); err != nil {
		ln.Close()
		return err
	}
	runner := codex.NewRunner(cfg, logger)
	go runner.RunPruner(ctx)
	// Every request context derives from baseCtx, so canceling it aborts all
	// in-flight codex runs (their process groups are killed and cleaned up).
	baseCtx, cancelRequests := context.WithCancelCause(context.Background())
	defer cancelRequests(nil)
	srv := &http.Server{
		Handler:           server.New(cfg, runner, logger).Handler(),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No ReadTimeout/WriteTimeout: a generation can legitimately take up to
		// codex.timeout, which is enforced by the runner instead.
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", ln.Addr().String(), "workspace", cfg.Workspace.Dir, "keep", cfg.Workspace.Keep.String(), "codex", cfg.Codex.Bin)
		errCh <- srv.Serve(ln)
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	grace := cfg.Server.ShutdownTimeout
	logger.Info("shutting down", "grace", grace)
	timer := time.AfterFunc(grace, func() {
		logger.Warn("grace period elapsed, canceling in-flight requests")
		cancelRequests(server.ErrShuttingDown)
	})
	defer timer.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace+cleanupGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
