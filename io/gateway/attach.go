package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aixodia-engine/imageprep"
	"aixodia-engine/provider"
)

// chatAttachment is one file the phone uploaded before sending its message. The
// daemon already stored it; this only names it so the picture inside can be sent
// to the model instead of being described to it.
type chatAttachment struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Mime string `json:"mime"`
}

// imageBudget is what one attached picture may cost in a request body. It is
// deliberately larger than the tool-result ceiling: a picture sent as an image
// part is billed as image tokens, not as the characters its base64 would take,
// so the cheap path is also the one that can afford detail.
const imageBudget = 1500 * 1024

// maxImagesPerMessage caps a single turn. Every picture costs tokens and time,
// and a message with a dozen of them is a mistake more often than an intent.
const maxImagesPerMessage = 8

// attachImages reads the pictures among the attachments and hangs them on the
// last user message, which is the one being asked.
//
// Only files inside the upload directory are opened. The path arrives from the
// phone, and a path is a request to read a file: without this check the model
// could be handed /etc/shadow by a message that says so.
//
// Anything that is not a picture is left alone. Those keep travelling as the
// text note the phone already sends, which tells the agent where the file is so
// it can read it with a tool.
func (g *Gateway) attachImages(messages []provider.Message, items []chatAttachment) ([]provider.Message, []string) {
	if len(items) == 0 || g.uploadDir == "" {
		return messages, nil
	}
	target := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			target = i
			break
		}
	}
	if target < 0 {
		return messages, nil
	}

	var images []provider.Image
	var skipped []string
	for _, item := range items {
		if len(images) >= maxImagesPerMessage {
			skipped = append(skipped, "too many pictures in one message")
			break
		}
		path, err := g.resolveUpload(item.Path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", nameOr(item), err))
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: cannot read it", nameOr(item)))
			continue
		}
		pic, err := imageprep.PrepareWithin(raw, imageBudget)
		if err != nil {
			// Not a picture, or not one this can decode. The text note already
			// covers it, so nothing is reported: a PDF is not a failure.
			continue
		}
		images = append(images, provider.Image{MediaType: pic.MediaType, Data: pic.Data})
	}
	if len(images) == 0 {
		return messages, skipped
	}

	// Copy rather than mutate: the caller still holds the original slice and
	// uses it to decide what the stored turn says.
	out := append([]provider.Message(nil), messages...)
	out[target].Images = append(out[target].Images, images...)
	return out, skipped
}

// resolveUpload turns a reported path into a real one, and refuses anything
// outside the upload directory.
func (g *Gateway) resolveUpload(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("no path")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute")
	}
	root, err := filepath.Abs(g.uploadDir)
	if err != nil {
		return "", fmt.Errorf("upload directory is unusable")
	}
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("path is unusable")
	}
	// EvalSymlinks so a symlink inside the upload directory cannot point out of
	// it. The file has to exist for this to work, which is also required.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("file not found")
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("outside the upload directory")
	}
	return resolved, nil
}

func nameOr(item chatAttachment) string {
	if strings.TrimSpace(item.Name) != "" {
		return item.Name
	}
	if base := filepath.Base(item.Path); base != "" && base != "." {
		return base
	}
	return "ไฟล์"
}
