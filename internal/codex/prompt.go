package codex

import (
	"fmt"
	"strings"
)

// Job describes one image generation or edit request handed to codex.
type Job struct {
	Prompt string
	// N is the number of final images to deliver. Codex may call the built-in
	// image_gen tool more often to replace drafts that miss the request.
	// 0 = unspecified: the prompt sets no count and every generated image is returned.
	N int
	// Transparent maps to the built-in tool's transparent_background argument.
	Transparent bool
	// AspectRatio such as "16:9", derived from the requested size; empty = unspecified.
	// The built-in tool has no size argument, so it is requested in the image_gen prompt.
	AspectRatio string
	// InputImages are absolute paths of saved input images, attached with -i;
	// non-empty means edit. They are not written into the prompt.
	InputImages []string
}

func (j Job) IsEdit() bool { return len(j.InputImages) > 0 }

// finalImageMarker prefixes each line of the agent's final message that names
// a delivered image; see finalImageIDs. The prompt does not ask for it: without
// such lines the runner keeps the last n images.
const finalImageMarker = "FINAL_IMAGE:"

// BuildPrompt renders the instructions passed to `codex exec` on stdin in two
// parts: job instructions for codex (the $imagegen skill mention, no questions,
// no files, the image count when n is given), then an "Image request" section holding what the
// image_gen prompt should carry: the image attributes the user's text cannot
// (aspect ratio, transparency) and the user's prompt verbatim. Keeping the count
// out of that section stops it from leaking into a single image as a grid.
//
// Everything else is left to the skill and the image_gen tool description: the
// skill defaults to the built-in tool, decides generate vs edit and iterates on
// misses; the tool sets transparent_background when the request asks for it and
// edits attached images through referenced_image_paths, since codex labels each
// -i attachment with its path. The runner collects whatever image_gen saved
// under $CODEX_HOME/generated_images/<thread_id>/, so no output location is named.
func BuildPrompt(job Job) string {
	var b strings.Builder
	b.WriteString("$imagegen\n")
	// codex exec cannot answer a question, and the skill would copy project-bound
	// images into the workspace.
	b.WriteString("Non-interactive preview job: don't ask questions, don't save or copy any files.\n")
	// Without a count codex delivers one file per variant a prompt such as
	// "4 logo variations" asks for, and the runner keeps only the last n.
	switch {
	case job.N == 1:
		b.WriteString("Deliver 1 image file, fulfilling the whole request on its own.\n")
	case job.N > 1:
		fmt.Fprintf(&b, "Deliver %d separate image files, each fulfilling the whole request on its own.\n", job.N)
	}

	var attrs []string
	if job.AspectRatio != "" {
		attrs = append(attrs, fmt.Sprintf("aspect ratio %s, %s", job.AspectRatio, orientation(job.AspectRatio)))
	}
	if job.Transparent {
		attrs = append(attrs, "transparent background")
	}
	b.WriteString("\nImage request")
	if len(attrs) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(attrs, "; "))
	}
	b.WriteString(":\n")
	b.WriteString(strings.TrimSpace(job.Prompt))
	b.WriteString("\n")
	return b.String()
}

// orientation describes a "W:H" ratio as landscape, portrait or square.
func orientation(ratio string) string {
	var w, h int
	if _, err := fmt.Sscanf(ratio, "%d:%d", &w, &h); err != nil || w == h {
		return "square"
	}
	if w > h {
		return "landscape"
	}
	return "portrait"
}
