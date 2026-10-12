package gateway

// Attachment uploads. The phone sends the raw bytes of a picked file and gets
// back the path the daemon saved it to. That path travels with the user's
// message, so the agent — which already has bash and read — can open the file
// itself. Nothing about the file's type needs to be understood here; images,
// video and documents all take the same route.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// maxUpload caps one attachment. The body is streamed to disk, never held in
// memory whole, and a file over the cap is refused rather than truncated.
const maxUpload = 128 << 20

// SetUploadDir names the directory attachments are saved under. An empty value
// disables uploads, which is the honest answer when the deployment has nowhere
// writable to put them.
func (g *Gateway) SetUploadDir(dir string) { g.uploadDir = dir }

// apiUploadAttachment stores one file and answers with where it went.
//
//	POST /v1/attachments?session_id=<id>&name=<file name>
func (g *Gateway) apiUploadAttachment(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(g.uploadDir) == "" {
		writeError(w, http.StatusServiceUnavailable, "uploads_disabled", "this daemon has no upload directory")
		return
	}
	session := safeSegment(r.URL.Query().Get("session_id"))
	if session == "" {
		session = "shared"
	}
	name := safeFileName(r.URL.Query().Get("name"))
	if name == "" {
		name = "upload.bin"
	}

	dir := filepath.Join(g.uploadDir, session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	dst := filepath.Join(dir, name)
	// Never overwrite: two files with the same name in one session are two
	// attachments, and the agent must be able to read both.
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		dst = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}

	f, err := os.Create(dst)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	defer f.Close()

	written, err := io.Copy(f, io.LimitReader(r.Body, maxUpload+1))
	if err != nil {
		_ = os.Remove(dst)
		writeError(w, http.StatusBadGateway, "storage_error", err.Error())
		return
	}
	if written > maxUpload {
		_ = os.Remove(dst)
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("ไฟล์ใหญ่เกิน %d MB", maxUpload>>20))
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"path": dst,
		"name": filepath.Base(dst),
		"size": written,
		"mime": strings.TrimSpace(r.Header.Get("Content-Type")),
	})
}

// safeSegment keeps one path element: letters, digits, dash and underscore.
// Anything else (a slash, a dot-dot) is dropped, so a caller cannot walk out of
// the upload directory.
func safeSegment(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// safeFileName keeps a readable file name while removing any directory part and
// characters that are awkward on a shell or a filesystem.
func safeFileName(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\\", "/")
	raw = raw[strings.LastIndex(raw, "/")+1:]
	raw = strings.ReplaceAll(raw, "\x00", "")
	raw = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return r
		case r == '.' || r == '-' || r == '_' || r == ' ' || r == '(' || r == ')':
			return r
		case r > 0x7F:
			// Non-ASCII letters are common in Thai file names; keep them.
			return r
		default:
			return '_'
		}
	}, raw)
	raw = strings.TrimSpace(raw)
	raw = strings.TrimLeft(raw, ".")
	if len(raw) > 120 {
		raw = raw[:120]
	}
	return raw
}
