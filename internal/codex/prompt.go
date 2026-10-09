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
// a delivered image; see finalImageIDs.
const finalImageMarker = "FINAL_IMAGE:"

// BuildPrompt renders the instructions passed to `codex exec` on stdin.
// The prompt starts with the $imagegen skill mention so codex loads that skill.
// It names no output location: the runner collects the images the built-in tool
// saves under $CODEX_HOME/generated_images/<thread_id>/, picking the ones the
// final message lists after finalImageMarker.
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
	images := "image"
	if job.N > 1 {
		images = "images"
	}
	fmt.Fprintf(&b, "Deliver exactly %d final %s for this request, each one the result of a built-in image_gen call", job.N, images)
	if job.Transparent {
		b.WriteString("; set transparent_background=true on every call")
	}
	b.WriteString(".\n")
	b.WriteString("You may call image_gen more than once per image to replace results that miss the request; ")
	b.WriteString("only the final images you list at the end are delivered.\n")

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
	b.WriteString("The images are collected automatically from where image_gen saves them: ")
	b.WriteString("do not copy, move, convert or save any image, and do not create or modify any files.\n")
	// fmt.Fprintf(&b, "End your final message with exactly %d line(s) of the form `%s <file name of the saved image>`, ", job.N, finalImageMarker)
	// b.WriteString("one per final image.\n\n")

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
