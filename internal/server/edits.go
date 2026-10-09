package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/chendefine/codex-image-api/internal/codex"
)

const multipartMemory = 32 << 20

var imageExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// editInput is the parsed edit request, independent of the body encoding.
type editInput struct {
	prompt     string
	n          *int
	background *string
	size       *string
	images     [][]byte
}

// edits implements POST /v1/images/edits for multipart/form-data and JSON bodies.
func (s *Server) edits(c *gin.Context) (*ImagesResponse, error) {
	var (
		in  *editInput
		err error
	)
	switch ct := c.ContentType(); {
	case ct == "multipart/form-data":
		in, err = s.parseMultipartEdit(c)
	case ct == "application/json" || strings.HasSuffix(ct, "+json"):
		in, err = s.parseJSONEdit(c)
	default:
		err = invalidRequest("", "Unsupported Content-Type %q; use multipart/form-data or application/json.", ct)
	}
	if err != nil {
		return nil, err
	}

	job, err := s.buildJob(in.prompt, in.n, in.background, in.size)
	if err != nil {
		return nil, err
	}
	if len(in.images) == 0 {
		return nil, invalidRequest("image", "Missing required parameter: 'image'.")
	}
	if len(in.images) > s.cfg.Limits.MaxImages {
		return nil, invalidRequest("image", "At most %d input images are allowed.", s.cfg.Limits.MaxImages)
	}

	wd, err := s.runner.NewWorkdir(c.GetString(requestIDKey))
	if err != nil {
		return nil, err
	}
	ctx := c.Request.Context()
	defer s.runner.Cleanup(ctx, wd)
	if job.InputImages, err = saveInputs(wd, in.images); err != nil {
		return nil, err
	}
	return s.execute(ctx, wd, job)
}

func (s *Server) parseMultipartEdit(c *gin.Context) (*editInput, error) {
	limit := int64(s.cfg.Limits.MaxImages+1)*s.cfg.Limits.MaxImageBytes + maxJSONBody
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	if err := c.Request.ParseMultipartForm(multipartMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, invalidRequest("", "Request body is too large.")
		}
		return nil, invalidRequest("", "Failed to parse multipart form: %v", err)
	}
	form := c.Request.MultipartForm
	in := &editInput{prompt: formValue(form, "prompt")}
	if v := formValue(form, "n"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, invalidRequest("n", "Invalid value for 'n': %q is not an integer.", v)
		}
		in.n = &n
	}
	if v, ok := form.Value["background"]; ok && len(v) > 0 {
		in.background = &v[0]
	}
	if v, ok := form.Value["size"]; ok && len(v) > 0 {
		in.size = &v[0]
	}

	// OpenAI SDKs send a single file as "image" and multiple files as "image[]".
	// "mask" is accepted but ignored: the built-in image_gen tool has no mask input.
	files := append(append([]*multipart.FileHeader{}, form.File["image"]...), form.File["image[]"]...)
	if len(files) > s.cfg.Limits.MaxImages {
		return nil, invalidRequest("image", "At most %d input images are allowed.", s.cfg.Limits.MaxImages)
	}
	for _, fh := range files {
		if fh.Size > s.cfg.Limits.MaxImageBytes {
			return nil, invalidRequest("image", "Image %q exceeds the %d byte limit.", fh.Filename, s.cfg.Limits.MaxImageBytes)
		}
		data, err := readFileHeader(fh)
		if err != nil {
			return nil, invalidRequest("image", "Failed to read image %q: %v", fh.Filename, err)
		}
		in.images = append(in.images, data)
	}
	return in, nil
}

func (s *Server) parseJSONEdit(c *gin.Context) (*editInput, error) {
	// Base64 data URLs are ~4/3 of the raw size.
	limit := int64(s.cfg.Limits.MaxImages+1)*s.cfg.Limits.MaxImageBytes*4/3 + maxJSONBody
	var req EditJSONRequest
	if err := decodeJSON(c, limit, &req); err != nil {
		return nil, err
	}
	in := &editInput{prompt: req.Prompt, n: req.N, background: req.Background, size: req.Size}
	if len(req.Images) > s.cfg.Limits.MaxImages {
		return nil, invalidRequest("images", "At most %d input images are allowed.", s.cfg.Limits.MaxImages)
	}
	for i, ref := range req.Images {
		param := fmt.Sprintf("images[%d]", i)
		data, err := s.loadImageReference(c.Request.Context(), ref, param)
		if err != nil {
			return nil, err
		}
		in.images = append(in.images, data)
	}
	return in, nil
}

func (s *Server) loadImageReference(ctx context.Context, ref ImageReference, param string) ([]byte, error) {
	switch {
	case ref.FileID != "":
		return nil, invalidRequest(param+".file_id", "'file_id' is not supported by this server; use 'image_url' instead.")
	case strings.HasPrefix(ref.ImageURL, "data:"):
		data, err := decodeDataURL(ref.ImageURL)
		if err != nil {
			return nil, invalidRequest(param+".image_url", "Invalid data URL: %v", err)
		}
		if int64(len(data)) > s.cfg.Limits.MaxImageBytes {
			return nil, invalidRequest(param+".image_url", "Image exceeds the %d byte limit.", s.cfg.Limits.MaxImageBytes)
		}
		return data, nil
	case strings.HasPrefix(ref.ImageURL, "http://") || strings.HasPrefix(ref.ImageURL, "https://"):
		if !s.cfg.Limits.AllowImageURLs {
			return nil, invalidRequest(param+".image_url", "Remote image URLs are disabled on this server; use a base64 data URL.")
		}
		return s.download(ctx, ref.ImageURL, param+".image_url")
	default:
		return nil, invalidRequest(param, "Each image must provide 'image_url' (a data URL or http(s) URL).")
	}
}

func (s *Server) download(ctx context.Context, url, param string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, invalidRequest(param, "Invalid image URL: %v", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, invalidRequest(param, "Failed to download image: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, invalidRequest(param, "Failed to download image: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.Limits.MaxImageBytes+1))
	if err != nil {
		return nil, invalidRequest(param, "Failed to download image: %v", err)
	}
	if int64(len(data)) > s.cfg.Limits.MaxImageBytes {
		return nil, invalidRequest(param, "Image exceeds the %d byte limit.", s.cfg.Limits.MaxImageBytes)
	}
	return data, nil
}

// saveInputs writes input images to <workdir>/input/image_<i>.<ext> and returns absolute paths.
func saveInputs(wd *codex.Workdir, images [][]byte) ([]string, error) {
	if err := os.Mkdir(wd.InputDir, 0o755); err != nil {
		return nil, fmt.Errorf("save input image: %w", err)
	}
	paths := make([]string, 0, len(images))
	for i, data := range images {
		ct := http.DetectContentType(data)
		ext, ok := imageExts[ct]
		if !ok {
			return nil, invalidRequest("image",
				"Invalid file format for image %d (%s). Supported formats are: 'png', 'jpeg', 'webp' and 'gif'.", i+1, ct)
		}
		p := filepath.Join(wd.InputDir, fmt.Sprintf("image_%d%s", i+1, ext))
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, fmt.Errorf("save input image: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func decodeDataURL(u string) ([]byte, error) {
	meta, payload, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ",")
	if !ok {
		return nil, errors.New("missing ',' separator")
	}
	if !strings.HasSuffix(meta, ";base64") {
		return nil, errors.New("only base64-encoded data URLs are supported")
	}
	payload = strings.TrimSpace(payload)
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if data, err2 := base64.RawStdEncoding.DecodeString(payload); err2 == nil {
			return data, nil
		}
		return nil, err
	}
	return data, nil
}

func formValue(form *multipart.Form, key string) string {
	if v := form.Value[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func readFileHeader(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
