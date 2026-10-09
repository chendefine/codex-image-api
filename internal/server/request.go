package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/chendefine/codex-image-api/internal/codex"
)

const (
	maxPromptChars = 32000
	maxSizeSide    = 100000
	maxJSONBody    = 1 << 20
)

// decodeJSON decodes exactly one JSON value of at most limit bytes from the
// request body. The trailing-data check reads the body to EOF, which lets
// net/http notice a client disconnect while codex runs.
func decodeJSON(c *gin.Context, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	err := dec.Decode(v)
	if err == nil {
		// Token, unlike More, also rejects a stray '}' or ']'.
		if _, tokErr := dec.Token(); tokErr != io.EOF {
			err = errors.New("unexpected data after JSON value")
			if errors.As(tokErr, new(*http.MaxBytesError)) {
				err = tokErr
			}
		}
	}
	var maxErr *http.MaxBytesError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &maxErr):
		return invalidRequest("", "Request body is too large.")
	case errors.Is(err, io.EOF):
		return invalidRequest("", "We could not parse the JSON body of your request: the body is empty")
	}
	return invalidRequest("", "We could not parse the JSON body of your request: %v", err)
}

// buildJob validates the parameters the built-in image_gen tool understands.
// size only contributes its aspect ratio: the tool cannot set exact dimensions.
func (s *Server) buildJob(prompt string, n *int, background, size *string) (codex.Job, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return codex.Job{}, invalidRequest("prompt", "Missing required parameter: 'prompt'.")
	}
	if utf8.RuneCountInString(prompt) > maxPromptChars {
		return codex.Job{}, invalidRequest("prompt", "'prompt' must be at most %d characters.", maxPromptChars)
	}
	job := codex.Job{Prompt: prompt, N: 1}
	if n != nil {
		if *n < 1 || *n > s.cfg.Limits.MaxN {
			return codex.Job{}, invalidRequest("n", "'n' must be between 1 and %d.", s.cfg.Limits.MaxN)
		}
		job.N = *n
	}
	if background != nil {
		switch *background {
		case "transparent":
			job.Transparent = true
		case "opaque", "auto", "":
		default:
			return codex.Job{}, invalidRequest("background",
				"Invalid value: '%s'. Supported values are: 'transparent', 'opaque', and 'auto'.", *background)
		}
	}
	if size != nil {
		ratio, err := sizeAspectRatio(*size)
		if err != nil {
			return codex.Job{}, err
		}
		job.AspectRatio = ratio
	}
	return job, nil
}

// sizeAspectRatio maps "WIDTHxHEIGHT" to an approximate aspect ratio;
// "auto" or empty means unspecified.
func sizeAspectRatio(size string) (string, error) {
	size = strings.TrimSpace(size)
	if size == "" || size == "auto" {
		return "", nil
	}
	ws, hs, ok := strings.Cut(strings.ToLower(size), "x")
	w, werr := strconv.Atoi(ws)
	h, herr := strconv.Atoi(hs)
	if !ok || werr != nil || herr != nil || w <= 0 || h <= 0 || w > maxSizeSide || h > maxSizeSide {
		return "", invalidRequest("size", "Invalid value: '%s'. Use 'auto' or 'WIDTHxHEIGHT', e.g. '1024x1024'.", size)
	}
	return codex.AspectRatio(w, h), nil
}
