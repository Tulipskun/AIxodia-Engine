package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ai-engine/provider"
)

type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, args string) (string, error)
}

func Builtin() map[string]Tool {
	bashDef := provider.ToolDef{
		Name:        "bash",
		Description: "Executes a shell command on the host (sh -c) and returns combined stdout and stderr output.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command to execute.",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds. Defaults to 60, capped at 300.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory. Defaults to /kaggle/working when it exists, else the process directory.",
				},
			},
			"required": []any{"command"},
		},
	}
	shellDef := bashDef
	shellDef.Name = "shell"
	m := map[string]Tool{
		"current_time": {
			Def: provider.ToolDef{
				Name:        "current_time",
				Description: "Returns the current date and time, optionally in a specific IANA time zone.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"timezone": map[string]any{
							"type":        "string",
							"description": "IANA time zone name, for example Asia/Bangkok. Defaults to UTC.",
						},
					},
				},
			},
			Run: currentTime,
		},
		"fetch_url": {
			Def: provider.ToolDef{
				Name:        "fetch_url",
				Description: "Fetches an http or https URL with GET and returns the response body as text.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{
							"type":        "string",
							"description": "Absolute http or https URL to fetch.",
						},
					},
					"required": []any{"url"},
				},
			},
			Run: fetchURL,
		},
		"bash":  {Def: bashDef, Run: runBash},
		"shell": {Def: shellDef, Run: runBash},
		"read": {
			Def: provider.ToolDef{
				Name:        "read",
				Description: "Reads a text file from the host filesystem and returns its contents, optionally a line range.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Absolute path of the file to read.",
						},
						"offset": map[string]any{
							"type":        "number",
							"description": "1-based first line to return. Defaults to 1.",
						},
						"limit": map[string]any{
							"type":        "number",
							"description": "Maximum lines to return. Defaults to 2000.",
						},
					},
					"required": []any{"path"},
				},
			},
			Run: readFile,
		},
	}
	return m
}

func runBash(ctx context.Context, args string) (string, error) {
	input := struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
		Workdir string  `json:"workdir"`
	}{}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(input.Command) == "" {
		return "", fmt.Errorf("command must not be empty")
	}
	timeout := 60 * time.Second
	if input.Timeout > 0 {
		timeout = time.Duration(input.Timeout * float64(time.Second))
		if timeout > 300*time.Second {
			timeout = 300 * time.Second
		}
		if timeout < time.Second {
			timeout = time.Second
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", input.Command)
	if dir := strings.TrimSpace(input.Workdir); dir != "" {
		cmd.Dir = dir
	} else if st, err := os.Stat("/kaggle/working"); err == nil && st.IsDir() {
		cmd.Dir = "/kaggle/working"
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	if len(text) > 32*1024 {
		text = text[:32*1024] + "\n[truncated]"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("command timed out after %s", timeout)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Sprintf("%sexit code %d", text, exitErr.ExitCode()), nil
		}
		return text, err
	}
	return text, nil
}

func readFile(ctx context.Context, args string) (string, error) {
	input := struct {
		Path   string  `json:"path"`
		Offset float64 `json:"offset"`
		Limit  float64 `json:"limit"`
	}{}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	p := strings.TrimSpace(input.Path)
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute")
	}
	clean := filepath.Clean(p)
	// Block virtual filesystems: /proc/self/environ and friends would leak
	// process secrets, and device files are never valid read targets.
	for _, blocked := range []string{"/proc", "/sys", "/dev"} {
		if clean == blocked || strings.HasPrefix(clean, blocked+"/") {
			return "", fmt.Errorf("reading %s is not allowed", blocked)
		}
	}
	st, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("cannot stat %s: %w", p, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	const maxBytes = int64(256 * 1024)
	if st.Size() > 4*1024*1024 {
		return "", fmt.Errorf("%s is too large (%.1f MB)", p, float64(st.Size())/1048576)
	}
	f, err := os.Open(clean)
	if err != nil {
		return "", fmt.Errorf("cannot open %s: %w", p, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", p, err)
	}
	text := string(raw)
	truncated := false
	if int64(len(raw)) > maxBytes {
		text = text[:maxBytes]
		truncated = true
	}
	offset := int(input.Offset)
	if offset < 1 {
		offset = 1
	}
	limit := int(input.Limit)
	if limit < 1 {
		limit = 2000
	}
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // drop phantom element from trailing newline
	}
	if offset > len(lines) {
		return "", fmt.Errorf("offset %d beyond end of file (%d lines)", offset, len(lines))
	}
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	out := strings.Join(lines[offset-1:end], "\n")
	if truncated || end < len(lines) {
		out += "\n[truncated]"
	}
	return out, nil
}

func currentTime(ctx context.Context, args string) (string, error) {
	input := struct {
		Timezone string `json:"timezone"`
	}{}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	location := time.UTC
	if input.Timezone != "" {
		parsed, err := time.LoadLocation(input.Timezone)
		if err != nil {
			return "", fmt.Errorf("unknown time zone %q", input.Timezone)
		}
		location = parsed
	}
	return time.Now().In(location).Format(time.RFC3339), nil
}

func fetchURL(ctx context.Context, args string) (string, error) {
	input := struct {
		URL string `json:"url"`
	}{}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(input.URL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("url must be absolute http or https")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: safePublicDialContext,
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if err := validatePublicURL(request.URL); err != nil {
				return err
			}
			return nil
		},
	}
	if err := validatePublicURL(parsed); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", provider.UserAgent)

	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 128*1024))
	if err != nil {
		return "", err
	}
	text := string(body)
	if len(text) > 32*1024 {
		text = text[:32*1024] + "\n[truncated]"
	}
	return fmt.Sprintf("HTTP %d\n%s", response.StatusCode, text), nil
}

// validatePublicURL rejects literal private and local addresses before dialing.
// safePublicDialContext repeats the check after DNS resolution to prevent
// DNS rebinding from reaching local services or cloud metadata endpoints.
func validatePublicURL(target *url.URL) error {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
		return fmt.Errorf("url must be absolute http or https")
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("local and private network URLs are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("local and private network URLs are not allowed")
	}
	return nil
}

func safePublicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("invalid port")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("hostname resolved to no addresses")
	}
	for _, entry := range ips {
		if !isPublicIP(entry.IP) {
			return nil, fmt.Errorf("local and private network URLs are not allowed")
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, entry := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(entry.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func isPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// Carrier-grade NAT and IPv4 special-use ranges are not public destinations.
	if v4 := ip.To4(); v4 != nil {
		blocked := []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"}
		for _, cidr := range blocked {
			_, network, _ := net.ParseCIDR(cidr)
			if network.Contains(v4) {
				return false
			}
		}
	}
	return true
}
