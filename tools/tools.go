package tools

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"syscall"
	"time"
	"unicode/utf8"

	"aixodia-engine/goals"
	"aixodia-engine/imageprep"
	"aixodia-engine/provider"
)

type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, args string) (string, error)
}

// sessionKey carries the chat id into a tool run. Goals and plans are scoped to
// the conversation that created them, and the tool signature has only a
// context to carry that scope.
type sessionKey struct{}

// WithSession marks a run as belonging to one chat. The gateway calls it once
// per turn; goal and plan tools refuse to run without it.
func WithSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionKey{}, strings.TrimSpace(sessionID))
}

func sessionOf(ctx context.Context) (string, error) {
	id, _ := ctx.Value(sessionKey{}).(string)
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("goal and plan tools need an active chat session")
	}
	return id, nil
}

// Builtin is the tool set the engine offers with no objective storage. It
// exists for callers that only need the declarations (the provider's free-tier
// contract injects shell and read); the running engine uses BuiltinWith.
func Builtin() map[string]Tool { return BuiltinWith(nil) }

func BuiltinWith(store goals.Store) map[string]Tool {
	// The command tool is "shell", not "bash": that is the name the real
	// OpenCode client (v2) sends — v1's "bash" was renamed to "shell" — and the
	// one the free-tier gate recognises. Offering "bash" as well only made the
	// model choose between two identical tools it then never used.
	shellDef := provider.ToolDef{
		Name: "shell",
		Description: "Executes a given shell command in a persistent shell session with optional timeout, " +
			"ensuring proper handling and security measures.\n\nBe aware: OS: linux, Shell: sh\n\n" +
			"All commands run in the current working directory by default. Use the `workdir` parameter if you " +
			"need to run a command in a different directory. AVOID using `cd <directory> && <command>` patterns - " +
			"use `workdir` instead.\n\nThe command argument is required. " +
			"Output combines stdout and stderr; a non-zero exit is reported as `exit code N` after the output. " +
			"If the output is very large it is truncated.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The command to execute.",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Optional timeout in milliseconds. Defaults to 120000, capped at 300000.",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "The working directory to run the command in. Defaults to the current directory. Use this instead of 'cd' commands.",
				},
			},
			"required": []any{"command"},
		},
	}
	readDef := provider.ToolDef{
		Name: "read",
		Description: "Read a file or directory from the local filesystem. If the path does not exist, an error is returned.\n\n" +
			"Usage:\n" +
			"- The filePath parameter should be an absolute path.\n" +
			"- By default, this tool returns up to 2000 lines from the start of the file.\n" +
			"- The offset parameter is the line number to start from (1-indexed).\n" +
			"- To read later sections, call this tool again with a larger offset.\n" +
			"- Contents are returned with each line prefixed by its line number as `<line>: <content>`.\n" +
			"- For directories, entries are returned one per line (without line numbers) with a trailing `/` for subdirectories.\n" +
			"- Any line longer than 2000 characters is truncated.\n" +
			"- Pictures cannot be read as text. Pass base64: true to have one shown to you as an image instead; it is shrunk first, and you receive the picture itself, not a base64 string.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filePath": map[string]any{
					"type":        "string",
					"description": "The absolute path to the file or directory to read.",
				},
				"offset": map[string]any{
					"type":        "number",
					"description": "The line number to start reading from (1-indexed). Defaults to 1.",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "The maximum number of lines to read (defaults to 2000).",
				},
				"base64": map[string]any{
					"type":        "boolean",
					"description": "Return the picture as an image you can see, instead of as text. Pictures are shrunk to a size a model can use first.",
				},
			},
			"required": []any{"filePath"},
		},
	}
	editDef := provider.ToolDef{
		Name:        "edit",
		Description: "Edit a file by replacing exact text. oldString must occur exactly once in the file. Returns a short confirmation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filePath": map[string]any{
					"type":        "string",
					"description": "Absolute path to the file to edit.",
				},
				"oldString": map[string]any{
					"type":        "string",
					"description": "Exact text to find and replace.",
				},
				"newString": map[string]any{
					"type":        "string",
					"description": "Text to replace oldString with.",
				},
			},
			"required": []any{"filePath", "oldString", "newString"},
		},
	}
	writeDef := provider.ToolDef{
		Name: "write",
		Description: "Write text to a file, creating it and any missing parent directories. " +
			"An existing file is replaced completely. Returns how many bytes were written.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filePath": map[string]any{
					"type":        "string",
					"description": "Absolute path to the file to write.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The complete new contents of the file.",
				},
			},
			"required": []any{"filePath", "content"},
		},
	}
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
		"shell": {Def: shellDef, Run: runBash},
		"read":  {Def: readDef, Run: readFile},
		"edit":  {Def: editDef, Run: runEdit},
		"write": {Def: writeDef, Run: runWrite},
	}
	for name, tool := range objectiveTools(store, goals.Goal, "goal") {
		m[name] = tool
	}
	for name, tool := range objectiveTools(store, goals.Plan, "plan") {
		m[name] = tool
	}
	for name, tool := range m {
		m[name] = withReason(tool)
	}
	return m
}

// objectiveTools builds the mirrored goal_* and plan_* tools. They differ only
// in the list they touch and the noun in their descriptions and output, so a
// plan cannot be completed through a goal tool or vice versa. These tools do
// not take a reason: changing a goal or plan is itself the intent.
func objectiveTools(store goals.Store, kind goals.Kind, noun string) map[string]Tool {
	idField := noun + "_id"
	addDescription := "Record the user's overall goal before working on it. Use this when the user asks " +
		"for something that will take several steps, so the objective survives across tool calls and turns. " +
		"Use plan_add for the separate steps. Returns the new " + idField + "."
	if kind == goals.Plan {
		addDescription = "Record one step toward the current goal. Plans are a separate list from goals. " +
			"Returns the new " + idField + "."
	}
	add := provider.ToolDef{
		Name:        noun + "_add",
		Description: addDescription,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{
					"type":        "string",
					"description": "What has to be achieved or done, in a short sentence.",
				},
			},
			"required": []any{"content"},
		},
	}
	edit := provider.ToolDef{
		Name:        noun + "_edit",
		Description: "Replace the content of an open " + noun + ". Use this when the user changes what it should be.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				idField: map[string]any{
					"type":        "string",
					"description": "The " + idField + " returned by " + noun + "_add.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The complete new content of the " + noun + ".",
				},
			},
			"required": []any{idField, "content"},
		},
	}
	complete := provider.ToolDef{
		Name: noun + "_complete",
		Description: "Mark a " + noun + " complete once its work is actually finished. " +
			"It is removed from the active list. Returns a confirmation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				idField: map[string]any{
					"type":        "string",
					"description": "The " + idField + " returned by " + noun + "_add.",
				},
			},
			"required": []any{idField},
		},
	}
	drop := provider.ToolDef{
		Name:        noun + "_drop",
		Description: "Mark a " + noun + " dropped when the user cancels it. It is removed from the active list. Returns a confirmation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				idField: map[string]any{
					"type":        "string",
					"description": "The " + idField + " returned by " + noun + "_add.",
				},
			},
			"required": []any{idField},
		},
	}
	// Both field names are decoded even though only one is used: the wire
	// argument is chosen at runtime by the helper, and a struct tag cannot be.
	type objectiveInput struct {
		GoalID  string `json:"goal_id"`
		PlanID  string `json:"plan_id"`
		Content string `json:"content"`
	}
	decode := func(args string) (objectiveInput, string, error) {
		var input objectiveInput
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			return input, "", fmt.Errorf("invalid arguments: %w", err)
		}
		id := input.GoalID
		if kind == goals.Plan {
			id = input.PlanID
		}
		return input, id, nil
	}
	fail := func() error {
		if store == nil {
			return fmt.Errorf("%s storage is not configured", noun)
		}
		return nil
	}
	return map[string]Tool{
		noun + "_add": {
			Def: add,
			Run: func(ctx context.Context, args string) (string, error) {
				input, _, err := decode(args)
				if err != nil {
					return "", err
				}
				if err := fail(); err != nil {
					return "", err
				}
				sessionID, err := sessionOf(ctx)
				if err != nil {
					return "", err
				}
				id, err := store.Add(sessionID, kind, input.Content)
				if err != nil {
					return "", err
				}
				return idField + ": " + id, nil
			},
		},
		noun + "_edit": {
			Def: edit,
			Run: func(ctx context.Context, args string) (string, error) {
				input, id, err := decode(args)
				if err != nil {
					return "", err
				}
				if err := fail(); err != nil {
					return "", err
				}
				sessionID, err := sessionOf(ctx)
				if err != nil {
					return "", err
				}
				if err := store.Edit(sessionID, kind, id, input.Content); err != nil {
					return "", err
				}
				return noun + " " + id + " updated", nil
			},
		},
		noun + "_complete": {
			Def: complete,
			Run: func(ctx context.Context, args string) (string, error) {
				_, id, err := decode(args)
				if err != nil {
					return "", err
				}
				if err := fail(); err != nil {
					return "", err
				}
				sessionID, err := sessionOf(ctx)
				if err != nil {
					return "", err
				}
				if err := store.Complete(sessionID, kind, id); err != nil {
					return "", err
				}
				return noun + " " + id + " completed", nil
			},
		},
		noun + "_drop": {
			Def: drop,
			Run: func(ctx context.Context, args string) (string, error) {
				_, id, err := decode(args)
				if err != nil {
					return "", err
				}
				if err := fail(); err != nil {
					return "", err
				}
				sessionID, err := sessionOf(ctx)
				if err != nil {
					return "", err
				}
				if err := store.Drop(sessionID, kind, id); err != nil {
					return "", err
				}
				return noun + " " + id + " dropped", nil
			},
		},
	}
}

// withReason adds the one parameter every ordinary tool shares: the model has
// to say briefly why it is calling the tool. Goal and plan tools are exempt:
// changing a goal or plan is the intent, and the reminder carries the list.
func withReason(tool Tool) Tool {
	if strings.HasPrefix(tool.Def.Name, "goal_") || strings.HasPrefix(tool.Def.Name, "plan_") {
		return tool
	}
	props, ok := tool.Def.Parameters["properties"].(map[string]any)
	if !ok {
		return tool
	}
	if _, exists := props["reason"]; exists {
		return tool
	}
	props["reason"] = map[string]any{
		"type": "string",
		"description": "A short reason for using this tool: why it is needed and " +
			"what it is meant to accomplish.",
	}
	required, _ := tool.Def.Parameters["required"].([]any)
	tool.Def.Parameters["required"] = append(required, "reason")
	return tool
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
	timeout := 120 * time.Second
	if input.Timeout > 0 {
		timeout = time.Duration(input.Timeout * float64(time.Millisecond))
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
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			cmd.Dir = home
		}
	}
	// Put the shell in its own process group so a timeout kills the whole
	// tree, not just the top-level shell (which would leave children running
	// and the call hanging).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the process group if the context is cancelled (timeout or caller).
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
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
		Path     string  `json:"path"`
		FilePath string  `json:"filePath"`
		Offset   float64 `json:"offset"`
		Limit    float64 `json:"limit"`
		Base64   bool    `json:"base64"`
	}{}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	p := strings.TrimSpace(input.FilePath)
	if p == "" {
		p = strings.TrimSpace(input.Path)
	}
	if p == "" {
		return "", fmt.Errorf("filePath must not be empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("filePath must be absolute")
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
		entries, err := os.ReadDir(clean)
		if err != nil {
			return "", fmt.Errorf("cannot list %s: %w", p, err)
		}
		var sb strings.Builder
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			sb.WriteString(name + "\n")
		}
		return sb.String(), nil
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	// The two paths want different amounts of the file, so they read it
	// themselves rather than sharing a window. Text is shown a slice; a picture
	// has to arrive whole, and a partial JPEG does not decode at all.
	if input.Base64 {
		return encodeBase64File(clean, st.Size())
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
	// A file that is not text has no lines and no valid UTF-8, so reading it
	// returns a screenful of replacement characters. The model reads that as a
	// glitch, tries the next tool, and after four rounds and ninety thousand
	// prompt tokens it still cannot see the picture — it just runs out of turn.
	// Saying what the file is, and that this tool only reads text, is the answer
	// that lets the model tell the user instead of trying again.
	if kind, binary := binaryKind(clean, raw); binary {
		return "", fmt.Errorf(
			"%s is a %s file, not text. The read tool only returns text, so the contents cannot be shown here.",
			p, kind,
		)
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
	const maxLineChars = 2000
	var sb strings.Builder
	for i, ln := range lines[offset-1 : end] {
		if len(ln) > maxLineChars {
			ln = ln[:maxLineChars]
		}
		fmt.Fprintf(&sb, "%d: %s\n", offset+i, ln)
	}
	out := sb.String()
	if truncated || end < len(lines) {
		out += "[truncated]"
	}
	return out, nil
}

func runEdit(ctx context.Context, args string) (string, error) {
	input := struct {
		Path      string `json:"path"`
		FilePath  string `json:"filePath"`
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
	}{}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	p := strings.TrimSpace(input.FilePath)
	if p == "" {
		p = strings.TrimSpace(input.Path)
	}
	if p == "" {
		return "", fmt.Errorf("filePath must not be empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("filePath must be absolute")
	}
	clean := filepath.Clean(p)
	for _, blocked := range []string{"/proc", "/sys", "/dev"} {
		if clean == blocked || strings.HasPrefix(clean, blocked+"/") {
			return "", fmt.Errorf("editing %s is not allowed", blocked)
		}
	}
	st, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("cannot stat %s: %w", p, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	raw, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", p, err)
	}
	text := string(raw)
	if text == input.OldString && input.OldString == input.NewString {
		return "", fmt.Errorf("oldString and newString must differ")
	}
	count := strings.Count(text, input.OldString)
	if count == 0 {
		return "", fmt.Errorf("oldString not found in %s", p)
	}
	if count > 1 {
		return "", fmt.Errorf("oldString occurs %d times in %s; add more context to make it unique", count, p)
	}
	newText := strings.Replace(text, input.OldString, input.NewString, 1)
	if err := os.WriteFile(clean, []byte(newText), st.Mode().Perm()); err != nil {
		return "", fmt.Errorf("cannot write %s: %w", p, err)
	}
	return fmt.Sprintf("edited %s: %d replacement(s)", p, count), nil
}

func runWrite(ctx context.Context, args string) (string, error) {
	input := struct {
		Path     string `json:"path"`
		FilePath string `json:"filePath"`
		Content  string `json:"content"`
	}{}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	p := strings.TrimSpace(input.FilePath)
	if p == "" {
		p = strings.TrimSpace(input.Path)
	}
	if p == "" {
		return "", fmt.Errorf("filePath must not be empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("filePath must be absolute")
	}
	clean := filepath.Clean(p)
	for _, blocked := range []string{"/proc", "/sys", "/dev"} {
		if clean == blocked || strings.HasPrefix(clean, blocked+"/") {
			return "", fmt.Errorf("writing %s is not allowed", blocked)
		}
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return "", fmt.Errorf("cannot create parent directory for %s: %w", p, err)
	}
	if err := os.WriteFile(clean, []byte(input.Content), 0o644); err != nil {
		return "", fmt.Errorf("cannot write %s: %w", p, err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(input.Content), p), nil
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

// binaryNames are the attachments a chat sends most often, named here so the
// refusal can say what the file is rather than only that it is unreadable.
var binaryNames = map[string]string{
	".jpg": "JPEG image", ".jpeg": "JPEG image", ".png": "PNG image",
	".gif": "GIF image", ".webp": "WebP image", ".bmp": "BMP image",
	".heic": "HEIC image", ".tiff": "TIFF image",
	".mp4": "MP4 video", ".mov": "QuickTime video", ".mkv": "Matroska video",
	".mp3": "MP3 audio", ".wav": "WAV audio", ".m4a": "M4A audio",
	".pdf": "PDF document", ".zip": "ZIP archive", ".gz": "gzip archive",
	".apk": "Android package", ".so": "shared library",
}

// binaryKind reports whether a file cannot be read as text, and what it is.
// The extension is checked first because it names the file exactly; the bytes
// are the fallback, because an extension proves nothing and the one thing that
// makes text unreadable — invalid UTF-8 or an embedded NUL — is in the bytes.
func binaryKind(path string, raw []byte) (string, bool) {
	if name, ok := binaryNames[strings.ToLower(filepath.Ext(path))]; ok {
		return name, true
	}
	head := raw
	if len(head) > 4096 {
		head = head[:4096]
	}
	if len(head) == 0 {
		return "", false
	}
	if bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(head) {
		return "binary", true
	}
	return "", false
}

// base64Ceiling caps what this returns as text. A prompt is charged by the
// character and base64 is four characters for every three bytes, so a 1 MB
// picture would arrive as 1.4 million characters — more than any context window
// holds and more than the picture is worth. The ceiling is what fits, and the
// header says so rather than truncating silently.
const base64Ceiling = 192 * 1024

// encodeBase64 returns a data URI for a file, shrinking a picture first.
//
// A picture is worth sending as base64 only after imageprep has made it small.
// The raw bytes of a phone screenshot encode to something no request can carry,
// and a model samples the same picture from far fewer pixels anyway.
// encodeBase64File reads a whole file and encodes it. The ceiling is far above
// the text window because a picture has to be complete to decode, and a modern
// phone photo is routinely 8 MB.
func encodeBase64File(path string, size int64) (string, error) {
	const maxEncodeBytes = int64(32 * 1024 * 1024)
	if size > maxEncodeBytes {
		return "", fmt.Errorf("%s is too large to encode (%.1f MB, ceiling %.0f MB)",
			path, float64(size)/1048576, float64(maxEncodeBytes)/1048576)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxEncodeBytes+1))
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	return encodeBase64(path, raw), nil
}

func encodeBase64(path string, raw []byte) string {
	// The ceiling counts characters and the picture is measured in bytes, and
	// base64 spends four characters on every three bytes, so the budget handed
	// to the shrinker is the ceiling scaled back.
	if pic, err := imageprep.PrepareWithin(raw, base64Ceiling*3/4); err == nil {
		note := fmt.Sprintf("%dx%d", pic.Width, pic.Height)
		if pic.Resized {
			note = fmt.Sprintf("%dx%d, reduced from %dx%d", pic.Width, pic.Height, pic.OriginalWidth, pic.OriginalHeight)
		}
		return dataURIResult(path, note, pic.MediaType, pic.Data)
	}
	return dataURIResult(path, fmt.Sprintf("%d bytes", len(raw)), guessMediaType(path), raw)
}

func dataURIResult(path, note, mediaType string, raw []byte) string {
	encoded := base64.StdEncoding.EncodeToString(raw)
	header := fmt.Sprintf("%s — %s (%s)", path, mediaType, note)
	if len(encoded) > base64Ceiling {
		return fmt.Sprintf(
			"%s\ntoo large to return: %d characters of base64, ceiling is %d. Use the bash tool to process it on the host instead.",
			header, len(encoded), base64Ceiling,
		)
	}
	return header + "\ndata:" + mediaType + ";base64," + encoded
}

// guessMediaType names a file by its extension, for the formats a chat sends.
func guessMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".txt", ".md", ".json", ".csv", ".log":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}
