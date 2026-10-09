package codex

import (
	"fmt"
	"strings"
)

// Job describes one image generation or edit request handed to codex.
type Job struct {
	Prompt string
	// N is the number of images to produce (one built-in image_gen call each).
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

// BuildPrompt renders the instructions passed to `codex exec` on stdin.
// The prompt starts with the $imagegen skill mention so codex loads that skill.
// It names no output location: the runner collects the images the built-in tool
// saves under $CODEX_HOME/generated_images/<thread_id>/.
func BuildPrompt(job Job) string {
	var b strings.Builder
	b.WriteString("$imagegen\n")
	b.WriteString("Use the imagegen skill in its default built-in `image_gen` tool mode. ")
	b.WriteString("This is a non-interactive job: never ask questions, proceed with sensible choices.\n\n")

	if job.IsEdit() {
		b.WriteString("Task: edit the input image(s) according to the request below.\n")
	} else {
		b.WriteString("Task: generate a brand-new image from the request below.\n")
	}
	calls := "call"
	if job.N > 1 {
		calls = "calls"
	}
	fmt.Fprintf(&b, "Make exactly %d separate built-in image_gen %s for this request", job.N, calls)
	if job.Transparent {
		b.WriteString("; set transparent_background=true")
	}
	b.WriteString(".\n")

	if job.IsEdit() {
		if k := len(job.InputImages); k == 1 {
			b.WriteString("The input image is attached to this message as [Image #1]; ")
		} else {
			fmt.Fprintf(&b, "The %d input images are attached to this message as [Image #1] ... [Image #%d]; ", k, k)
		}
		b.WriteString("use them, in that order, as the edit targets of every image_gen call ")
		b.WriteString("(pass the attached images' file paths as referenced_image_paths).\n")
	}

	if job.AspectRatio != "" {
		fmt.Fprintf(&b, "Every output image must have a %s (width:height) aspect ratio, %s; ", job.AspectRatio, orientation(job.AspectRatio))
		b.WriteString("state this aspect ratio explicitly in every image_gen prompt.\n")
	}

	b.WriteString("Use the full request text as the image_gen prompt: do not shorten it or drop any constraint. ")
	b.WriteString("You may add structure per the skill guidance, but do not add new subjects.\n")
	// b.WriteString("The generated images are collected automatically: do not copy, move or save them anywhere, ")
	b.WriteString("Do not create or modify any other files.\n\n")

	b.WriteString("Request:\n<<<\n")
	b.WriteString(strings.TrimSpace(job.Prompt))
	b.WriteString("\n>>>\n")
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
