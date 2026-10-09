package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/chendefine/codex-image-api/internal/logctx"
)

const (
	requestIDHeader       = "X-Request-Id"
	clientRequestIDHeader = "X-Client-Request-Id"
	requestIDKey          = "request_id"
	maxClientRequestID    = 128
)

var lastRequestID atomic.Uint64

// newRequestID returns 16 hex digits: the Unix time in milliseconds (11
// digits, 44 bits, enough until the year 2527), a 2-digit sequence within that
// millisecond and 3 random digits, e.g. 1a120aaf66a00b7c.
// Timestamp and sequence are strictly increasing within the process, so IDs
// never collide and workdirs and log lines sort chronologically; beyond 256
// IDs in one millisecond the sequence carries into the next one.
func newRequestID() string {
	now := uint64(time.Now().UnixMilli()) & (1<<44 - 1) << 8
	for {
		last := lastRequestID.Load()
		next := max(now, last+1)
		if lastRequestID.CompareAndSwap(last, next) {
			return fmt.Sprintf("%013x%03x", next, rand.IntN(1<<12))
		}
	}
}

// requestID assigns every request an ID (see newRequestID), exposed in the
// X-Request-Id response header and used as the name of its workdir. The
// request context carries a logger annotated with it (and with
// X-Client-Request-Id, if the client sent one), so log lines from the server
// and the codex runner can be correlated.
func (s *Server) requestID(c *gin.Context) {
	id := newRequestID()
	c.Set(requestIDKey, id)
	c.Header(requestIDHeader, id)

	logger := s.logger.With(requestIDKey, id)
	if cid := c.GetHeader(clientRequestIDHeader); cid != "" {
		logger = logger.With("client_request_id", sanitizeHeader(cid, maxClientRequestID))
	}
	c.Request = c.Request.WithContext(logctx.With(c.Request.Context(), logger))
}

// sanitizeHeader keeps printable ASCII only and truncates to max bytes, so a
// client-supplied value stays short and readable in the logs.
func sanitizeHeader(v string, max int) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, v)
	if len(v) > max {
		v = v[:max]
	}
	return v
}

// log returns the request-scoped logger.
func (s *Server) log(c *gin.Context) *slog.Logger {
	return logctx.From(c.Request.Context(), s.logger)
}

// accessLog writes one line per request after it completes.
func (s *Server) accessLog(c *gin.Context) {
	start := time.Now()
	c.Next()
	if c.Request.URL.Path == "/healthz" {
		return
	}
	s.log(c).Info("request",
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"status", c.Writer.Status(),
		"latency", time.Since(start).Round(time.Millisecond),
		"client_ip", c.ClientIP(),
	)
}

// recovery turns handler panics into a logged 500 with an OpenAI error body.
func (s *Server) recovery(c *gin.Context) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			panic(v)
		}
		s.log(c).Error("panic", "panic", v, "stack", string(debug.Stack()))
		if c.Writer.Written() {
			c.Abort()
			return
		}
		writeAPIError(c, &APIError{Status: http.StatusInternalServerError, Type: "server_error",
			Message: "internal server error"})
	}()
	c.Next()
}

// auth enforces "Authorization: Bearer <key>" when server.api_keys is configured.
// Keys are compared as SHA-256 digests against every configured key, so the
// time taken reveals neither the key length nor which key matched.
func (s *Server) auth(c *gin.Context) {
	if len(s.keyHashes) == 0 {
		return
	}
	scheme, token, _ := strings.Cut(c.GetHeader("Authorization"), " ")
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	match := 0
	for _, h := range s.keyHashes {
		match |= subtle.ConstantTimeCompare(sum[:], h[:])
	}
	if match == 1 && strings.EqualFold(scheme, "Bearer") {
		return
	}
	c.Header("WWW-Authenticate", `Bearer realm="codex-image-api"`)
	s.writeError(c, &APIError{Status: http.StatusUnauthorized, Type: "invalid_request_error",
		Code: "invalid_api_key", Message: "Incorrect API key provided."})
}
