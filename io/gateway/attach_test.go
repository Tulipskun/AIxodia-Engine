package gateway

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aixodia-engine/provider"
)

func writeJPEG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for i := 0; i < 40*30; i++ {
		img.Set(i%40, i/40, color.RGBA{uint8(i * 3), 90, 200, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAttachImagesHangsAPictureOnTheLastUserMessage(t *testing.T) {
	uploads := t.TempDir()
	pic := filepath.Join(uploads, "s1", "photo.jpg")
	writeJPEG(t, pic)

	g := &Gateway{uploadDir: uploads}
	messages := []provider.Message{
		{Role: "user", Content: "อันเก่า"},
		{Role: "assistant", Content: "ตอบ"},
		{Role: "user", Content: "รูปนี้ใช่คนเดียวกันมั้ย?"},
	}
	got, skipped := g.attachImages(messages, []chatAttachment{{Path: pic, Name: "photo.jpg", Mime: "image/jpeg"}})

	if len(skipped) != 0 {
		t.Errorf("nothing should have been skipped: %v", skipped)
	}
	if len(got[2].Images) != 1 {
		t.Fatalf("the picture belongs on the message being asked, got %d images there", len(got[2].Images))
	}
	if got[0].Images != nil {
		t.Error("an earlier user message must not gain the picture")
	}
	if got[2].Images[0].MediaType != "image/jpeg" {
		t.Errorf("media type = %q", got[2].Images[0].MediaType)
	}
	// The caller still holds the original slice, and uses it to decide what the
	// stored turn says; mutating it would put bytes in the transcript.
	if messages[2].Images != nil {
		t.Error("attachImages must not mutate the caller's slice")
	}
}

// A path arrives from the phone, and a path is a request to read a file. Without
// the directory check, a message could ask for /etc/shadow and get it.
func TestAttachImagesRefusesPathsOutsideTheUploadDirectory(t *testing.T) {
	uploads := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.jpg")
	writeJPEG(t, secret)

	g := &Gateway{uploadDir: uploads}
	messages := []provider.Message{{Role: "user", Content: "อ่านไฟล์นี้"}}
	got, skipped := g.attachImages(messages, []chatAttachment{{Path: secret, Name: "secret.jpg"}})

	if len(got[0].Images) != 0 {
		t.Fatal("a file outside the upload directory must never be attached")
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "outside") {
		t.Errorf("the refusal should say why: %v", skipped)
	}
}

// The check resolves symlinks, so a link planted inside the upload directory
// cannot be used to step out of it.
func TestAttachImagesRefusesASymlinkOutOfTheDirectory(t *testing.T) {
	uploads := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.jpg")
	writeJPEG(t, secret)
	link := filepath.Join(uploads, "innocent.jpg")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	g := &Gateway{uploadDir: uploads}
	got, skipped := g.attachImages(
		[]provider.Message{{Role: "user", Content: "x"}},
		[]chatAttachment{{Path: link, Name: "innocent.jpg"}},
	)
	if len(got[0].Images) != 0 {
		t.Fatal("a symlink pointing out of the upload directory must be refused")
	}
	if len(skipped) != 1 {
		t.Errorf("expected one refusal, got %v", skipped)
	}
}

func TestAttachImagesLeavesOtherFilesToTheTextNote(t *testing.T) {
	uploads := t.TempDir()
	doc := filepath.Join(uploads, "report.pdf")
	if err := os.WriteFile(doc, []byte("%PDF-1.7 not a picture"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := &Gateway{uploadDir: uploads}
	got, skipped := g.attachImages(
		[]provider.Message{{Role: "user", Content: "สรุปไฟล์นี้"}},
		[]chatAttachment{{Path: doc, Name: "report.pdf", Mime: "application/pdf"}},
	)
	if len(got[0].Images) != 0 {
		t.Error("a PDF is not a picture and must not become an image part")
	}
	// Not reported as a failure either: the note already tells the agent where
	// the file is, so there is nothing to complain about.
	if len(skipped) != 0 {
		t.Errorf("a non-picture is not an error: %v", skipped)
	}
}

func TestAttachImagesCapsHowManyOneMessageMayCarry(t *testing.T) {
	uploads := t.TempDir()
	items := make([]chatAttachment, 0, maxImagesPerMessage+3)
	for i := 0; i < maxImagesPerMessage+3; i++ {
		p := filepath.Join(uploads, "p", string(rune('a'+i))+".jpg")
		writeJPEG(t, p)
		items = append(items, chatAttachment{Path: p, Name: "p.jpg"})
	}

	g := &Gateway{uploadDir: uploads}
	got, skipped := g.attachImages([]provider.Message{{Role: "user", Content: "x"}}, items)
	if len(got[0].Images) != maxImagesPerMessage {
		t.Errorf("attached %d pictures, want the cap of %d", len(got[0].Images), maxImagesPerMessage)
	}
	if len(skipped) == 0 {
		t.Error("the ones left out should be reported")
	}
}

func TestAttachImagesDoesNothingWithoutAnUploadDirectory(t *testing.T) {
	g := &Gateway{}
	got, _ := g.attachImages(
		[]provider.Message{{Role: "user", Content: "x"}},
		[]chatAttachment{{Path: "/nonexistent/uploads/a.jpg"}},
	)
	if got[0].Images != nil {
		t.Error("with uploads disabled nothing may be read from disk")
	}
}
