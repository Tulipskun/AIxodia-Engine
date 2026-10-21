package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aixodia-engine/imageprep"
)

func TestValidatePublicURLRejectsLocalTargets(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://localhost/",
		"http://service.local/",
	} {
		t.Run(raw, func(t *testing.T) {
			target, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := validatePublicURL(target); err == nil {
				t.Fatalf("validatePublicURL(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestValidatePublicURLAllowsPublicHostnames(t *testing.T) {
	for _, raw := range []string{"https://example.com/", "http://8.8.8.8/"} {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validatePublicURL(target); err != nil {
			t.Errorf("validatePublicURL(%q): %v", raw, err)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"10.1.2.3", false},
		{"100.64.0.1", false},
		{"169.254.169.254", false},
		{"192.0.2.1", false},
		{"::1", false},
		{"fd00::1", false},
		{"2606:4700:4700::1111", true},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			if got := isPublicIP(net.ParseIP(tc.ip)); got != tc.want {
				t.Errorf("isPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestBuiltinOffersShellAndRead(t *testing.T) {
	b := Builtin()
	for _, name := range []string{
		"current_time", "fetch_url", "shell", "read", "edit", "write",
		"goal_add", "goal_edit", "goal_complete", "goal_drop",
	} {
		tool, ok := b[name]
		if !ok {
			t.Fatalf("Builtin() missing tool %q", name)
		}
		if tool.Def.Name != name {
			t.Errorf("tool %q has Def.Name %q", name, tool.Def.Name)
		}
		if tool.Run == nil {
			t.Errorf("tool %q has nil Run", name)
		}
		props, _ := tool.Def.Parameters["properties"].(map[string]any)
		if _, ok := props["reason"]; !ok {
			t.Errorf("tool %q has no reason parameter", name)
		}
		required, _ := tool.Def.Parameters["required"].([]any)
		found := false
		for _, r := range required {
			if r == "reason" {
				found = true
			}
		}
		if !found {
			t.Errorf("tool %q does not require reason", name)
		}
	}
	// The v1 name must be gone: the command tool is "shell" only, matching the
	// real OpenCode client, and leaving "bash" offered gave the model two
	// identical tools to choose between.
	if _, ok := b["bash"]; ok {
		t.Error("Builtin() still offers the old \"bash\" tool")
	}
}

func TestRunWriteCreatesParentsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "file.txt")

	out, err := runWrite(context.Background(), `{"filePath":"`+path+`","content":"first"}`)
	if err != nil {
		t.Fatalf("runWrite: %v", err)
	}
	if !strings.Contains(out, "wrote 5 bytes") {
		t.Errorf("runWrite = %q", out)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "first" {
		t.Fatalf("after write got %q, %v", got, err)
	}

	if _, err := runWrite(context.Background(), `{"path":"`+path+`","content":"second"}`); err != nil {
		t.Fatalf("runWrite overwrite: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "second" {
		t.Errorf("overwrite left %q", got)
	}
}

func TestRunWriteRejectsBadPaths(t *testing.T) {
	for _, args := range []string{
		`{"filePath":"relative.txt","content":"x"}`,
		`{"filePath":"","content":"x"}`,
		`{"filePath":"/proc/self/mem","content":"x"}`,
		`{"filePath":"/sys/kernel/x","content":"x"}`,
	} {
		if _, err := runWrite(context.Background(), args); err == nil {
			t.Errorf("runWrite(%s) unexpectedly succeeded", args)
		}
	}
}

type fakeGoals struct {
	content string
	edited  string
	status  string
}

func (f *fakeGoals) Add(content string) (string, error) {
	f.content = content
	return "g-test", nil
}

func (f *fakeGoals) Edit(id, content string) error {
	f.edited = id + ":" + content
	return nil
}

func (f *fakeGoals) Complete(id string) error {
	f.status = "done:" + id
	return nil
}

func (f *fakeGoals) Drop(id string) error {
	f.status = "dropped:" + id
	return nil
}

func TestGoalToolsUseTheStore(t *testing.T) {
	store := &fakeGoals{}
	b := BuiltinWith(store)

	out, err := b["goal_add"].Run(context.Background(), `{"content":"ship the fix"}`)
	if err != nil {
		t.Fatalf("goal_add: %v", err)
	}
	if out != "goal_id: g-test" || store.content != "ship the fix" {
		t.Errorf("goal_add = %q, stored %q", out, store.content)
	}

	if _, err := b["goal_edit"].Run(context.Background(), `{"goal_id":"g-test","content":"ship it well"}`); err != nil {
		t.Fatalf("goal_edit: %v", err)
	}
	if store.edited != "g-test:ship it well" {
		t.Errorf("goal_edit stored %q", store.edited)
	}

	if _, err := b["goal_complete"].Run(context.Background(), `{"goal_id":"g-test"}`); err != nil {
		t.Fatalf("goal_complete: %v", err)
	}
	if store.status != "done:g-test" {
		t.Errorf("goal_complete stored %q", store.status)
	}

	if _, err := b["goal_drop"].Run(context.Background(), `{"goal_id":"g-test"}`); err != nil {
		t.Fatalf("goal_drop: %v", err)
	}
	if store.status != "dropped:g-test" {
		t.Errorf("goal_drop stored %q", store.status)
	}
}

func TestGoalToolsWithoutStoreFail(t *testing.T) {
	b := Builtin()
	if _, err := b["goal_add"].Run(context.Background(), `{"content":"x"}`); err == nil {
		t.Error("goal_add without a store unexpectedly succeeded")
	}
}

func TestRunBashEcho(t *testing.T) {
	out, err := runBash(context.Background(), `{"command":"echo hello-bash"}`)
	if err != nil {
		t.Fatalf("runBash: %v", err)
	}
	if out != "hello-bash\n" {
		t.Errorf("runBash = %q, want %q", out, "hello-bash\n")
	}
}

func TestRunBashNonZeroExitReturnsOutput(t *testing.T) {
	out, err := runBash(context.Background(), `{"command":"echo oops >&2; exit 3"}`)
	if err != nil {
		t.Fatalf("runBash: %v", err)
	}
	if out != "oops\nexit code 3" {
		t.Errorf("runBash = %q, want %q", out, "oops\nexit code 3")
	}
}

func TestRunBashEmptyCommandRejected(t *testing.T) {
	if _, err := runBash(context.Background(), `{"command":"  "}`); err == nil {
		t.Error("runBash with empty command unexpectedly succeeded")
	}
}

func TestRunBashTimeout(t *testing.T) {
	_, err := runBash(context.Background(), `{"command":"sleep 30","timeout":1}`)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("runBash timeout: got %v, want timeout error", err)
	}
}

func TestReadFileRange(t *testing.T) {
	f, err := os.CreateTemp("", "readtest-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	content := "l1\nl2\nl3\nl4\nl5\n"
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out, err := readFile(context.Background(), `{"filePath":"`+f.Name()+`","offset":2,"limit":2}`)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if out != "2: l2\n3: l3\n[truncated]" {
		t.Errorf("readFile = %q", out)
	}
	out, err = readFile(context.Background(), `{"path":"`+f.Name()+`"}`)
	if err != nil {
		t.Fatalf("readFile full (path alias): %v", err)
	}
	if out != "1: l1\n2: l2\n3: l3\n4: l4\n5: l5\n" {
		t.Errorf("readFile full = %q", out)
	}
}

func TestReadFileBlocksSensitivePaths(t *testing.T) {
	for _, p := range []string{"/proc/self/environ", "/sys/kernel/hostname", "/dev/null", "relative/path.txt", "/nonexistent-xyz-123"} {
		if _, err := readFile(context.Background(), `{"path":"`+p+`"}`); err == nil {
			t.Errorf("readFile(%q) unexpectedly succeeded", p)
		}
	}
}

func TestReadFileDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/a.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := readFile(context.Background(), `{"filePath":"`+dir+`"}`)
	if err != nil {
		t.Fatalf("readFile dir: %v", err)
	}
	if out != "a.txt\nsub/\n" {
		t.Errorf("readFile dir = %q", out)
	}
}

// Reading a picture used to return a screenful of replacement characters, which
// the model read as a glitch: it tried four more tools and burned ninety
// thousand prompt tokens still unable to see the image. The tool has to say what
// the file is instead.
func TestReadRefusesBinaryAndNamesIt(t *testing.T) {
	dir := t.TempDir()

	jpeg := filepath.Join(dir, "photo.jpg")
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xFF}
	if err := os.WriteFile(jpeg, jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readFile(context.Background(), `{"filePath":"`+jpeg+`"}`)
	if err == nil {
		t.Fatal("reading a JPEG should fail, it is not text")
	}
	if !strings.Contains(err.Error(), "JPEG image") {
		t.Errorf("the refusal should name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "not text") {
		t.Errorf("the refusal should say the tool only reads text: %v", err)
	}

	// A file that lies about its extension is caught on the bytes.
	lying := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(lying, []byte{0x7F, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00, 0xFF, 0xFE}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(context.Background(), `{"filePath":"`+lying+`"}`); err == nil {
		t.Error("a file with an NUL byte and broken UTF-8 should fail even named .txt")
	}
}

func TestReadStillWorksForText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readFile(context.Background(), `{"filePath":"`+path+`"}`)
	if err != nil {
		t.Fatalf("a text file should still read: %v", err)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("got %q", got)
	}
}

func TestBinaryKindNamesWhatItFound(t *testing.T) {
	cases := []struct {
		path string
		raw  []byte
		want string
		bin  bool
	}{
		{"a.pdf", []byte("%PDF-1.7"), "PDF document", true},
		{"a.PNG", []byte("anything"), "PNG image", true},
		{"a.txt", []byte("plain text\n"), "", false},
		{"a.txt", []byte{'a', 0x00, 'b'}, "binary", true},
		{"a.txt", []byte{0xC3, 0x28}, "binary", true},
		{"a.txt", []byte(""), "", false},
	}
	for _, c := range cases {
		got, bin := binaryKind(c.path, c.raw)
		if bin != c.bin || got != c.want {
			t.Errorf("binaryKind(%q) = %q,%v want %q,%v", c.path, got, bin, c.want, c.bin)
		}
	}
}

// base64 is the only way a picture can travel through a tool result, and it has
// to arrive shrunk: the raw bytes of a screenshot encode to more characters than
// a prompt can hold.
func TestReadBase64ShrinksAPicture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.jpg")

	img := image.NewRGBA(image.Rect(0, 0, 2400, 1600))
	for i := 0; i < 2400*1600; i++ {
		img.Set(i%2400, i/2400, color.RGBA{uint8(i), uint8(i / 7), 90, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFile(context.Background(), `{"filePath":"`+path+`","base64":true}`)
	if err != nil {
		t.Fatalf("base64 read: %v", err)
	}
	if !strings.HasPrefix(got, path) {
		t.Errorf("the result should name the file first:\n%.80s", got)
	}
	if !strings.Contains(got, "reduced from 2400x1600") {
		t.Errorf("the result should say the picture was shrunk:\n%.160s", got)
	}
	if !strings.Contains(got, "data:image/jpeg;base64,") {
		t.Errorf("the result should be a data URI a caller can use:\n%.160s", got)
	}
	if len(got) > base64Ceiling+400 {
		t.Errorf("result is %d characters, over the ceiling", len(got))
	}
	// Decode it back with the standard library rather than trusting the prefix:
	// a data URI that says image/jpeg but holds something else is worse than an
	// obvious failure.
	idx := strings.Index(got, "base64,")
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(got[idx+len("base64,"):]))
	if err != nil {
		t.Fatalf("the returned base64 does not decode: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("the decoded bytes are not a JPEG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() > imageprep.MaxEdge || b.Dy() > imageprep.MaxEdge {
		t.Errorf("decoded picture is %dx%d, over the %d limit", b.Dx(), b.Dy(), imageprep.MaxEdge)
	}
}

// Without the flag the old refusal still stands, so a model that ignores the
// parameter is told what happened rather than handed replacement characters.
func TestReadWithoutBase64StillRefusesAPicture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.jpg")
	if err := os.WriteFile(path, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(context.Background(), `{"filePath":"`+path+`"}`); err == nil {
		t.Error("a picture without base64:true should still be refused")
	}
}

// A file too big to encode is reported as too big, not truncated: half a base64
// string decodes to half a picture, which is worse than none.
func TestReadBase64RefusesWhatItCannotCarry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	raw := make([]byte, 400*1024)
	for i := range raw {
		raw[i] = byte(i * 31)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readFile(context.Background(), `{"filePath":"`+path+`","base64":true}`)
	if err != nil {
		t.Fatalf("a large file should answer, not fail: %v", err)
	}
	if !strings.Contains(got, "too large to return") {
		t.Errorf("expected a size refusal, got:\n%.200s", got)
	}
	if strings.Contains(got, ";base64,") {
		t.Error("nothing should be encoded when it does not fit")
	}
}
