package gateway

import (
	"encoding/base64"
	"fmt"
	"strings"

	"ai-engine/provider"
)

// A tool can hand a picture back only as text, and the one form both tool
// authors and the wire shapes agree on is a base64 data URI. Sent on as text it
// is worth nothing: a provider reads picture bytes from an image part, never
// from a string, so the model is blind to it and the base64 only fills the
// context. extractToolImages lifts those URIs out of a tool result, leaving a
// short marker behind, so the caller can re-attach the pictures as images the
// model can actually see.
//
// The picture cannot ride on the tool result itself: the Responses API takes a
// string for a function's output and the chat-completions tool role has no
// image part, so the caller sends it as its own message right after the result.
const (
	// maxToolImages caps one tool round. Each picture costs image tokens, and a
	// result carrying a dozen is a mistake more often than an intent.
	maxToolImages = 8
	// maxToolImageBytes caps the decoded size a single round may add. The read
	// tool shrinks a picture before encoding it; a hand-built URI might not.
	maxToolImageBytes = 4 << 20
)

// extractToolImages replaces every image data URI in text with a marker and
// returns the decoded pictures in order. Anything past a cap is marked as
// omitted rather than attached, so one result cannot flood a request. Non-image
// data URIs and malformed ones are left exactly as they were.
func extractToolImages(text string) (string, []provider.Image) {
	var (
		images []provider.Image
		out    strings.Builder
		total  int
		rest   = text
	)
	for {
		start := strings.Index(rest, "data:image/")
		if start < 0 {
			break
		}
		media, data, span, ok := parseImageDataURI(rest[start:])
		if !ok {
			// Not a picture URI (or not decodable): keep the text and move past
			// its "data:" so the next search cannot match the same spot again.
			out.WriteString(rest[:start+len("data:")])
			rest = rest[start+len("data:"):]
			continue
		}
		out.WriteString(rest[:start])
		switch {
		case len(images) >= maxToolImages:
			out.WriteString("[picture omitted: this turn already carries enough]")
		case total+len(data) > maxToolImageBytes:
			out.WriteString("[picture omitted: too large for this turn]")
		default:
			images = append(images, provider.Image{MediaType: media, Data: data})
			total += len(data)
			out.WriteString(fmt.Sprintf("[picture attached below: %s]", media))
		}
		rest = rest[start+span:]
	}
	out.WriteString(rest)
	return out.String(), images
}

// parseImageDataURI parses a base64 image data URI at the start of s and
// reports the media type, the decoded bytes, and how many bytes of s the URI
// spans. It refuses anything but an image and anything whose payload does not
// decode, because a data URI that claims to be a picture and holds something
// else is worse than leaving the text alone.
func parseImageDataURI(s string) (media string, data []byte, span int, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", nil, 0, false
	}
	comma := strings.Index(s, ";base64,")
	if comma < 0 {
		return "", nil, 0, false
	}
	media = s[len(prefix):comma]
	if !strings.HasPrefix(media, "image/") {
		return "", nil, 0, false
	}
	payload := comma + len(";base64,")
	end := payload
	for end < len(s) && isBase64Byte(s[end]) {
		end++
	}
	if end == payload {
		return "", nil, 0, false
	}
	decoded, err := base64.StdEncoding.DecodeString(s[payload:end])
	if err != nil || len(decoded) == 0 {
		return "", nil, 0, false
	}
	return media, decoded, end, true
}

func isBase64Byte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '+', c == '/', c == '=':
		return true
	}
	return false
}
