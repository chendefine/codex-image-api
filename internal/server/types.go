package server

// GenerationRequest is the JSON body of POST /v1/images/generations.
// Only prompt, n, background and size (as an aspect ratio) reach the codex
// built-in image_gen tool; the remaining OpenAI parameters are accepted and ignored.
type GenerationRequest struct {
	Prompt     string  `json:"prompt"`
	N          *int    `json:"n"`
	Background *string `json:"background"`
	Size       *string `json:"size"`

	Model             *string `json:"model"`
	Quality           *string `json:"quality"`
	Moderation        *string `json:"moderation"`
	OutputFormat      *string `json:"output_format"`
	OutputCompression *int    `json:"output_compression"`
	PartialImages     *int    `json:"partial_images"`
	ResponseFormat    *string `json:"response_format"`
	Stream            *bool   `json:"stream"`
	Style             *string `json:"style"`
	User              *string `json:"user"`
}

// ImageReference is an input image in a JSON edit request.
type ImageReference struct {
	FileID   string `json:"file_id"`
	ImageURL string `json:"image_url"`
}

// EditJSONRequest is the JSON body variant of POST /v1/images/edits.
type EditJSONRequest struct {
	Prompt     string           `json:"prompt"`
	Images     []ImageReference `json:"images"`
	N          *int             `json:"n"`
	Background *string          `json:"background"`
	Size       *string          `json:"size"`

	Mask              *ImageReference `json:"mask"`
	InputFidelity     *string         `json:"input_fidelity"`
	Model             *string         `json:"model"`
	Quality           *string         `json:"quality"`
	Moderation        *string         `json:"moderation"`
	OutputFormat      *string         `json:"output_format"`
	OutputCompression *int            `json:"output_compression"`
	PartialImages     *int            `json:"partial_images"`
	Stream            *bool           `json:"stream"`
	User              *string         `json:"user"`
}

// ImagesResponse mirrors the OpenAI ImagesResponse object.
type ImagesResponse struct {
	Created      int64       `json:"created"`
	Background   string      `json:"background,omitempty"`
	Data         []ImageData `json:"data"`
	OutputFormat string      `json:"output_format,omitempty"`
	Size         string      `json:"size,omitempty"`
}

type ImageData struct {
	B64JSON string `json:"b64_json"`
}
