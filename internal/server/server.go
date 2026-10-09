// Package server exposes the OpenAI-compatible image endpoints.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "golang.org/x/image/webp"

	"github.com/chendefine/codex-image-api/internal/codex"
	"github.com/chendefine/codex-image-api/internal/config"
	"github.com/chendefine/codex-image-api/internal/logctx"
)

type Server struct {
	cfg        *config.Config
	runner     *codex.Runner
	logger     *slog.Logger
	httpClient *http.Client
	// keyHashes are the SHA-256 digests of server.api_keys.
	keyHashes [][sha256.Size]byte
}

func New(cfg *config.Config, runner *codex.Runner, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:        cfg,
		runner:     runner,
		logger:     logger,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
	for _, k := range cfg.Server.APIKeys {
		s.keyHashes = append(s.keyHashes, sha256.Sum256([]byte(k)))
	}
	return s
}

// Handler builds the gin router with all routes registered.
// Every error, including 404/405, auth failures and panics, is rendered as an
// OpenAI error envelope.
func (s *Server) Handler() http.Handler {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.Use(s.requestID, s.accessLog, s.recovery)
	r.NoRoute(func(c *gin.Context) {
		s.writeError(c, &APIError{Status: http.StatusNotFound, Type: "invalid_request_error",
			Message: fmt.Sprintf("Unknown request URL: %s %s", c.Request.Method, c.Request.URL.Path)})
	})
	r.NoMethod(func(c *gin.Context) {
		s.writeError(c, &APIError{Status: http.StatusMethodNotAllowed, Type: "invalid_request_error",
			Message: fmt.Sprintf("Method %s is not allowed for %s", c.Request.Method, c.Request.URL.Path)})
	})

	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	v1 := r.Group("/v1", s.auth)
	v1.POST("/images/generations", handle(s, s.generations))
	v1.POST("/images/edits", handle(s, s.edits))
	return r
}

// handle adapts an endpoint that returns its result instead of writing it:
// the result is rendered as JSON, an error as an OpenAI error envelope.
func handle[T any](s *Server, fn func(c *gin.Context) (T, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		rsp, err := fn(c)
		if err != nil {
			s.writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, rsp)
	}
}

// execute runs codex in wd and converts the images to an ImagesResponse.
func (s *Server) execute(ctx context.Context, wd *codex.Workdir, job codex.Job) (*ImagesResponse, error) {
	images, err := s.runner.Run(ctx, wd, job)
	if err != nil {
		return nil, err
	}
	rsp := &ImagesResponse{Created: time.Now().Unix(), Background: "opaque"}
	if job.Transparent {
		rsp.Background = "transparent"
	}
	for _, img := range images {
		rsp.Data = append(rsp.Data, ImageData{B64JSON: base64.StdEncoding.EncodeToString(img.Data)})
	}
	first := images[0]
	rsp.OutputFormat = strings.TrimPrefix(first.ContentType, "image/")
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(first.Data)); err == nil {
		rsp.Size = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	}
	if len(images) < job.N {
		logctx.From(ctx, s.logger).Warn("fewer images than requested", "dir", wd.Dir, "want", job.N, "got", len(images))
	}
	return rsp, nil
}
