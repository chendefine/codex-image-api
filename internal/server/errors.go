package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/chendefine/codex-image-api/internal/codex"
)

// ErrShuttingDown is the cancel cause of request contexts aborted because the
// server is shutting down; such requests are answered with 503.
var ErrShuttingDown = errors.New("server is shutting down")

// APIError is rendered as the OpenAI error envelope.
type APIError struct {
	Status  int
	Message string
	Type    string
	Param   string
	Code    string
	// RetryAfter, if set, is sent as the Retry-After header (whole seconds).
	RetryAfter int
}

func (e *APIError) Error() string { return e.Message }

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func (e *APIError) body() errorBody {
	d := errorDetail{Message: e.Message, Type: e.Type}
	if e.Param != "" {
		d.Param = &e.Param
	}
	if e.Code != "" {
		d.Code = &e.Code
	}
	return errorBody{Error: d}
}

func invalidRequest(param, format string, args ...any) *APIError {
	return &APIError{
		Status:  http.StatusBadRequest,
		Message: fmt.Sprintf(format, args...),
		Type:    "invalid_request_error",
		Param:   param,
	}
}

// toAPIError maps handler errors to OpenAI-style errors.
func toAPIError(err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	var runErr *codex.RunError
	switch {
	case errors.Is(err, codex.ErrTimeout):
		return &APIError{Status: http.StatusGatewayTimeout, Type: "server_error", Code: "timeout",
			Message: "image generation timed out"}
	case errors.Is(err, ErrShuttingDown):
		return &APIError{Status: http.StatusServiceUnavailable, Type: "server_error", Code: "shutting_down",
			Message: "server is shutting down, please retry", RetryAfter: 5}
	case errors.Is(err, codex.ErrBusy):
		return &APIError{Status: http.StatusServiceUnavailable, Type: "server_error", Code: "server_busy",
			Message: "the server is busy with other image requests, please retry later", RetryAfter: 30}
	case errors.Is(err, context.Canceled):
		return &APIError{Status: 499, Type: "server_error", Code: "canceled", Message: "request canceled"}
	case errors.As(err, &runErr):
		msg := runErr.Err.Error()
		if runErr.LastMessage != "" {
			msg += ": " + runErr.LastMessage
		}
		return &APIError{Status: http.StatusInternalServerError, Type: "server_error", Code: "image_generation_failed",
			Message: msg}
	}
	return &APIError{Status: http.StatusInternalServerError, Type: "server_error", Message: "internal server error"}
}

// writeError renders err as an OpenAI error envelope, logs server-side
// failures and aborts the chain. Every error response goes through it, except
// for panics, which recovery has already logged and renders with writeAPIError.
func (s *Server) writeError(c *gin.Context, err error) {
	// The runner returns the cancel cause itself, but other code paths (e.g.
	// downloading an input image) surface a plain context.Canceled.
	if errors.Is(err, context.Canceled) {
		if cause := context.Cause(c.Request.Context()); errors.Is(cause, ErrShuttingDown) {
			err = cause
		}
	}
	apiErr := toAPIError(err)
	if apiErr.Status >= 500 {
		s.log(c).Error("request failed", "status", apiErr.Status, "err", err)
	}
	writeAPIError(c, apiErr)
}

func writeAPIError(c *gin.Context, e *APIError) {
	if e.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	c.AbortWithStatusJSON(e.Status, e.body())
}
