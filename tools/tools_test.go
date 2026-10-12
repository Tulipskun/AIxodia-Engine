package tools

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
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

func TestBuiltinHasBashAndRead(t *testing.T) {
	b := Builtin()
	for _, name := range []string{"current_time", "fetch_url", "bash", "shell", "read"} {
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
