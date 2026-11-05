package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"opsagent/internal/config"
)

// These ceilings apply even when a caller bypasses config validation.
const (
	MaxFileBytes      = 1 << 20
	MaxOutputBytes    = 64 << 10
	MaxPreviewBytes   = 64 << 10
	MaxArgumentsBytes = 2 << 20
	MaxWalkEntries    = 2000
	MaxSearchBytes    = 8 << 20
	MaxSearchDepth    = 16
	MaxReadLines      = 2000
)

var (
	ErrDenied      = errors.New("tool access denied")
	ErrStale       = errors.New("prepared operation changed; prepare and obtain approval again")
	ErrUnsupported = errors.New("operation unsupported on this platform")
	ErrTooLarge    = errors.New("tool resource limit exceeded")
	ErrInvalidCall = errors.New("invalid tool arguments")
)

type Definition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type Call struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Result struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
}

// Prepared is a sealed, one-runner capability, not proof of user approval.
// The caller MUST obtain approval before Execute when NeedsApproval is true.
// Exported fields are display-only; modifying them invalidates the capability.
// Mutating operations can be executed at most once, including failed attempts.
// Prepared deliberately cannot be restored from JSON: prepare again on restart.
type Prepared struct {
	Call          Call
	NeedsApproval bool
	Preview       string
	state         *preparedState
}

type preparedState struct {
	mu         sync.Mutex
	owner      *Runner
	call       Call
	preview    string
	approval   bool
	used       bool
	group      bool
	dryRun     bool
	args       arguments
	path       string
	policyKey  string
	original   fileVersion
	contents   []byte
	preparedAt time.Time
}

type Runner struct {
	getConfig      func() config.Config
	protected      []string
	protectedInfo  []os.FileInfo
	invalidProtect bool
	writeMu        sync.Mutex
}

func New(getConfig func() config.Config, protectedDirs []string) *Runner {
	r := &Runner{getConfig: getConfig}
	for _, p := range protectedDirs {
		if !filepath.IsAbs(p) || strings.IndexByte(p, 0) >= 0 {
			r.invalidProtect = true
			continue
		}
		p = filepath.Clean(p)
		r.protected = append(r.protected, p)
		// Resolution is ONLY used to add deny rules, never to authorize/open.
		if real, err := filepath.EvalSymlinks(p); err == nil {
			r.protected = append(r.protected, real)
		}
		if fi, err := os.Stat(p); err == nil {
			r.protectedInfo = append(r.protectedInfo, fi)
		}
	}
	return r
}

func (r *Runner) configuration() config.Config {
	if r.getConfig == nil {
		return config.Config{}
	}
	return r.getConfig()
}

func (r *Runner) Definitions(group bool) []Definition {
	if group {
		return []Definition{}
	}
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	integer := func(description string, min, max int) map[string]any {
		return map[string]any{"type": "integer", "description": description, "minimum": min, "maximum": max}
	}
	path := text("Absolute path or path relative to work_dir; authorized roots only. Symlinks, credentials and protected directories are denied.")
	def := func(name, description string, props map[string]any, required ...string) Definition {
		return Definition{Name: name, Description: description, Parameters: map[string]any{
			"type": "object", "properties": props, "required": append([]string{}, required...), "additionalProperties": false,
		}}
	}
	return []Definition{
		def("system_status", "Read host CPU, memory and disk metrics on Linux. Other platforms return actual Go-process metrics and an explicit unsupported list, not simulated host data.", map[string]any{}),
		def("list_processes", "List bounded process names/PIDs, never command lines or environment. Non-Linux returns only this Go process with unsupported system_process_list.", map[string]any{"limit": integer("Maximum processes (default 50)", 1, 200)}),
		def("service_status", "Linux only: read selected systemd unit state properties via systemctl show --no-pager --plain. Does not read logs, start services or run a shell.", map[string]any{"service": text("Strict unit name: letters, digits, dot, underscore, @ and hyphen; must begin with a letter or digit; at most 128 characters")}, "service"),
		def("read_logs", "Read the last lines of a bounded UTF-8 log FILE within read_roots. Journal/journalctl is not supported. Maximum file size 1 MiB.", map[string]any{"path": path, "lines": integer("Tail lines (default 200)", 1, MaxReadLines)}, "path"),
		def("list_directory", "List a bounded directory, excluding symlinks, credential paths and protected directories. Does not recurse.", map[string]any{"path": path, "limit": integer("Maximum entries (default 100)", 1, 1000)}),
		def("read_file", "Read a 1-based inclusive line range from a UTF-8 file of at most 1 MiB. Default 200 lines; output is always bounded.", map[string]any{"path": path, "start_line": integer("First line (default 1)", 1, MaxFileBytes), "end_line": integer("Last line (default start_line+199); at most 2000 lines", 1, MaxFileBytes)}, "path"),
		def("search_files", "Search UTF-8 file contents for a literal Unicode substring (not a regex or shell command). At most 2000 entries, depth 16, 8 MiB total scanned, 1 MiB/file; excludes sensitive paths and all links.", map[string]any{"path": path, "query": text("Non-empty literal substring, at most 512 bytes"), "case_sensitive": map[string]any{"type": "boolean", "description": "Default false"}, "max_results": integer("Maximum matching lines (default 50)", 1, 200)}, "query"),
		def("write_file", "APPROVAL REQUIRED. Create or overwrite a UTF-8 file in BOTH read_roots and write_roots. Prepare includes full bounded diff and original SHA-256; execute rechecks permissions and original file identity/hash. Existing parent required. Denied in dry-run.", map[string]any{"path": path, "content": text("Complete new UTF-8 content, at most 1 MiB; changes too large to display in full are rejected")}, "path", "content"),
		def("replace_text", "APPROVAL REQUIRED. Literal text replacement in a UTF-8 file in BOTH read_roots and write_roots. By default old_text must occur exactly once; all=true replaces every match. Hash checked again immediately before write. Denied in dry-run.", map[string]any{"path": path, "old_text": text("Non-empty literal text"), "new_text": text("Replacement text"), "all": map[string]any{"type": "boolean"}}, "path", "old_text", "new_text"),
		def("exec", "APPROVAL REQUIRED; Linux only; denied in dry-run. Run /bin/sh in a new process group with fixed PATH/LANG and NO inherited secrets/HOME/BASH_ENV/ENV. Cwd is validated, but arbitrary shell is NOT sandboxed: read/write roots and protected_dirs do NOT constrain shell access; only the OS account permissions do. Review the exact command. Timeout terminates then kills its process group.", map[string]any{"command": text("Exact shell command, at most 8192 bytes"), "cwd": path}, "command"),
	}
}

type arguments struct {
	Path          string `json:"path"`
	Content       string `json:"content"`
	OldText       string `json:"old_text"`
	NewText       string `json:"new_text"`
	All           bool   `json:"all"`
	Command       string `json:"command"`
	Cwd           string `json:"cwd"`
	Service       string `json:"service"`
	Query         string `json:"query"`
	CaseSensitive bool   `json:"case_sensitive"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	Lines         int    `json:"lines"`
	Limit         int    `json:"limit"`
	MaxResults    int    `json:"max_results"`
}

func decodeArguments(call Call) (arguments, error) {
	var a arguments
	if len(call.Arguments) > MaxArgumentsBytes {
		return a, ErrTooLarge
	}
	// Check names against the actual schema as the shared struct is a superset.
	raw := call.Arguments
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return a, fmt.Errorf("%w: arguments must be an object", ErrInvalidCall)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	_, err := dec.Token()
	if err != nil {
		return a, fmt.Errorf("%w: malformed JSON", ErrInvalidCall)
	}
	keys := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return a, fmt.Errorf("%w: malformed JSON", ErrInvalidCall)
		}
		key, ok := token.(string)
		if !ok || keys[key] {
			return a, fmt.Errorf("%w: duplicate/invalid argument name", ErrInvalidCall)
		}
		keys[key] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return a, fmt.Errorf("%w: invalid or null argument", ErrInvalidCall)
		}
	}
	if _, err := dec.Token(); err != nil {
		return a, fmt.Errorf("%w: malformed JSON", ErrInvalidCall)
	}
	if _, err := dec.Token(); err != io.EOF {
		return a, fmt.Errorf("%w: trailing JSON", ErrInvalidCall)
	}
	var definition *Definition
	for _, d := range (&Runner{}).Definitions(false) {
		if d.Name == call.Name {
			copy := d
			definition = &copy
			break
		}
	}
	if definition == nil {
		return a, fmt.Errorf("%w: unknown tool", ErrInvalidCall)
	}
	props := definition.Parameters["properties"].(map[string]any)
	for k := range keys {
		if _, ok := props[k]; !ok {
			return a, fmt.Errorf("%w: unexpected argument %q", ErrInvalidCall, k)
		}
	}
	for _, k := range definition.Parameters["required"].([]string) {
		if !keys[k] {
			return a, fmt.Errorf("%w: missing %s", ErrInvalidCall, k)
		}
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("%w: wrong argument type", ErrInvalidCall)
	}
	return a, nil
}

var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}$`)

func (r *Runner) Prepare(call Call, group bool, dryRun bool) (*Prepared, error) {
	if group {
		return nil, fmt.Errorf("%w: no local tools in group conversations", ErrDenied)
	}
	a, err := decodeArguments(call)
	if err != nil {
		return nil, err
	}
	mutating := call.Name == "write_file" || call.Name == "replace_text" || call.Name == "exec"
	if mutating && dryRun {
		return nil, fmt.Errorf("%w: writes and shell execution are disabled in dry-run", ErrDenied)
	}
	cfg := r.configuration()
	pol := r.policy(cfg)
	s := &preparedState{owner: r, call: cloneCall(call), approval: mutating, group: group, dryRun: dryRun, args: a, policyKey: pol.key(), preparedAt: time.Now()}
	s.preview = call.Name
	switch call.Name {
	case "system_status":
	case "list_processes":
		if a.Limit < 0 || a.Limit > 200 {
			return nil, ErrInvalidCall
		}
	case "service_status":
		if !serviceName.MatchString(a.Service) {
			return nil, fmt.Errorf("%w: invalid service name", ErrInvalidCall)
		}
		s.preview = "Read systemd state for " + a.Service
	case "read_file", "read_logs", "write_file", "replace_text":
		if a.Path == "" {
			return nil, ErrInvalidCall
		}
		if a.StartLine < 0 || a.EndLine < 0 || a.StartLine > MaxFileBytes || a.EndLine > MaxFileBytes || a.Lines < 0 || a.Lines > MaxReadLines {
			return nil, ErrInvalidCall
		}
		start, end := lineRange(a)
		if end < start || end-start+1 > MaxReadLines {
			return nil, ErrInvalidCall
		}
		path, err := pol.absolute(a.Path)
		if err != nil {
			return nil, err
		}
		s.path = path
		old, version, err := pol.readFile(context.Background(), path, mutating, call.Name == "write_file")
		if err != nil {
			return nil, err
		}
		s.original = version
		if mutating {
			if !validText(old) || !validText([]byte(a.Content)) || !validText([]byte(a.NewText)) || !validText([]byte(a.OldText)) {
				return nil, fmt.Errorf("%w: text must be UTF-8 without NUL", ErrInvalidCall)
			}
			updated := []byte(a.Content)
			if call.Name == "replace_text" {
				if a.OldText == "" {
					return nil, fmt.Errorf("%w: old_text is empty", ErrInvalidCall)
				}
				count := strings.Count(string(old), a.OldText)
				if count == 0 || (!a.All && count != 1) {
					return nil, fmt.Errorf("%w: old_text must match once unless all=true (matches: %d)", ErrInvalidCall, count)
				}
				newSize := int64(len(old)) + int64(count)*(int64(len(a.NewText))-int64(len(a.OldText)))
				if newSize > MaxFileBytes {
					return nil, ErrTooLarge
				}
				updated = []byte(strings.ReplaceAll(string(old), a.OldText, a.NewText))
			}
			if len(updated) > MaxFileBytes {
				return nil, ErrTooLarge
			}
			if version.exists && bytes.Equal(old, updated) {
				return nil, fmt.Errorf("%w: proposed edit makes no change", ErrInvalidCall)
			}
			s.contents = append([]byte(nil), updated...)
			s.preview, err = editPreview(path, old, updated, version)
			if err != nil {
				return nil, err
			}
		} else {
			if !validText(old) {
				return nil, fmt.Errorf("%w: only UTF-8 text files are supported", ErrInvalidCall)
			}
			s.preview = fmt.Sprintf("Read %s\nOriginal SHA-256: %s", path, version.hash)
		}
	case "list_directory", "search_files":
		if a.Limit < 0 || a.Limit > 1000 || a.MaxResults < 0 || a.MaxResults > 200 {
			return nil, ErrInvalidCall
		}
		if call.Name == "search_files" && (a.Query == "" || len(a.Query) > 512 || !validText([]byte(a.Query))) {
			return nil, ErrInvalidCall
		}
		path, err := pol.absolute(defaultString(a.Path, "."))
		if err != nil {
			return nil, err
		}
		f, err := pol.openDirectory(path)
		if err != nil {
			return nil, err
		}
		f.Close()
		s.path = path
		s.preview = call.Name + " " + path
	case "exec":
		if !shellSupported() {
			return nil, fmt.Errorf("%w: exec is Linux-only", ErrUnsupported)
		}
		if strings.TrimSpace(a.Command) == "" || len(a.Command) > 8192 || strings.IndexByte(a.Command, 0) >= 0 || !utf8.ValidString(a.Command) {
			return nil, ErrInvalidCall
		}
		path, err := pol.absolute(defaultString(a.Cwd, "."))
		if err != nil {
			return nil, err
		}
		f, err := pol.openDirectory(path)
		if err != nil {
			return nil, err
		}
		s.original.info, err = f.Stat()
		f.Close()
		if err != nil {
			return nil, err
		}
		s.path = path
		s.preview = fmt.Sprintf("SHELL APPROVAL — NOT AN OS SANDBOX\nWorking directory: %s\nTimeout: %ds\nEnvironment: fixed PATH and locale; no inherited HOME, credentials, BASH_ENV or ENV.\nFile-tool roots/protected directories do NOT restrict shell commands. Only this OS account's permissions protect data.\nExact /bin/sh command:\n%s", path, commandSeconds(cfg), a.Command)
	default:
		return nil, ErrInvalidCall
	}
	return &Prepared{Call: cloneCall(s.call), NeedsApproval: mutating, Preview: s.preview, state: s}, nil
}

func (r *Runner) Execute(ctx context.Context, p *Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if p == nil || p.state == nil || p.state.owner != r {
		return Result{}, ErrStale
	}
	s := p.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.NeedsApproval != s.approval || p.Preview != s.preview || p.Call.Name != s.call.Name || !bytes.Equal(p.Call.Arguments, s.call.Arguments) || s.group || (s.approval && s.dryRun) || s.used {
		return Result{}, ErrStale
	}
	if s.approval {
		s.used = true
		r.writeMu.Lock()
		defer r.writeMu.Unlock()
	}
	cfg := r.configuration()
	pol := r.policy(cfg)
	if s.approval && pol.key() != s.policyKey {
		return Result{}, fmt.Errorf("%w: permissions/configuration changed", ErrStale)
	}
	limit := outputLimit(cfg)
	switch s.call.Name {
	case "system_status":
		m, err := Sample(ctx, cfg.WorkDir)
		b, marshalErr := json.Marshal(m)
		if marshalErr != nil {
			return Result{}, marshalErr
		}
		return boundedResult(string(b), limit, false), err
	case "list_processes":
		return listProcesses(ctx, defaultInt(s.args.Limit, 50), limit)
	case "service_status":
		if !serviceName.MatchString(s.args.Service) {
			return Result{}, ErrInvalidCall
		}
		return serviceStatus(ctx, s.args.Service, commandSeconds(cfg), limit)
	case "read_file", "read_logs":
		b, v, err := pol.readFile(ctx, s.path, false, false)
		if err != nil {
			return Result{}, err
		}
		if !s.original.equal(v) {
			return Result{}, ErrStale
		}
		return readLines(b, s.args, s.call.Name == "read_logs", limit), nil
	case "list_directory":
		return pol.listDirectory(ctx, s.path, defaultInt(s.args.Limit, 100), limit)
	case "search_files":
		return pol.searchFiles(ctx, s.path, s.args, limit)
	case "write_file", "replace_text":
		if err := pol.writeFile(ctx, s.path, s.original, s.contents); err != nil {
			return Result{}, err
		}
		return boundedResult(fmt.Sprintf("Wrote %d bytes to %s\nSHA-256: %s", len(s.contents), s.path, hashBytes(s.contents)), limit, false), nil
	case "exec":
		f, err := pol.openDirectory(s.path)
		if err != nil {
			return Result{}, err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || s.original.info == nil || !os.SameFile(s.original.info, fi) {
			return Result{}, ErrStale
		}
		return executeShell(ctx, s.args.Command, f, commandSeconds(cfg), limit)
	}
	return Result{}, ErrInvalidCall
}

func outputLimit(c config.Config) int {
	if c.Limits.OutputBytes <= 0 {
		return 16384
	}
	return min(c.Limits.OutputBytes, MaxOutputBytes)
}

func commandSeconds(c config.Config) int {
	if c.Limits.CommandSeconds <= 0 {
		return 60
	}
	return min(c.Limits.CommandSeconds, 300)
}

func cloneCall(c Call) Call {
	return Call{Name: c.Name, Arguments: append(json.RawMessage(nil), c.Arguments...)}
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func defaultInt(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

func validText(b []byte) bool { return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 }

func boundedResult(text string, limit int, truncated bool) Result {
	if len(text) > limit {
		text = text[:limit]
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		truncated = true
	}
	return Result{Output: text, Truncated: truncated}
}

// limitedBuffer drains command pipes even after the output budget is exhausted.
// Write always reports consumption, avoiding unbounded child-pipe backpressure.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := max(0, b.limit-b.buf.Len())
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) result() Result {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Shell output may not be UTF-8. Replacement never expands past the cap.
	return boundedResult(strings.ToValidUTF8(b.buf.String(), "?"), b.limit, b.truncated)
}
