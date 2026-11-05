package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"opsagent/internal/config"
)

func newTestRunner(t *testing.T) (*Runner, *config.Config, string) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("file tools fail closed on this platform")
	}
	dir := t.TempDir()
	cfg := config.Default(dir)
	return New(func() config.Config { return cfg }, nil), &cfg, dir
}

func toolCall(t *testing.T, name string, args map[string]any) Call {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Call{Name: name, Arguments: b}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runTool(t *testing.T, r *Runner, name string, args map[string]any) Result {
	t.Helper()
	p, err := r.Prepare(toolCall(t, name, args), false, false)
	if err != nil {
		t.Fatalf("prepare %s: %v", name, err)
	}
	res, err := r.Execute(context.Background(), p)
	if err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return res
}

func TestDefinitionsAndInputValidation(t *testing.T) {
	r, _, _ := newTestRunner(t)
	if len(r.Definitions(true)) != 0 {
		t.Fatal("group tools must be empty")
	}
	if len(r.Definitions(false)) != 10 {
		t.Fatal("unexpected tool count")
	}
	if _, err := r.Prepare(Call{Name: "system_status", Arguments: []byte("{}")}, true, false); !errors.Is(err, ErrDenied) {
		t.Fatalf("group: %v", err)
	}
	cases := []Call{
		{Name: "unknown", Arguments: []byte("{}")},
		{Name: "system_status", Arguments: []byte("null")},
		{Name: "system_status", Arguments: []byte("{} {}")},
		{Name: "read_file", Arguments: []byte(`{"path":"x","path":"y"}`)},
		{Name: "read_file", Arguments: []byte(`{"path":null}`)},
		{Name: "read_file", Arguments: []byte(`{"path":"x","command":"oops"}`)},
		{Name: "read_file", Arguments: []byte(`{"Path":"x"}`)},
		{Name: "service_status", Arguments: []byte(`{"service":"-a"}`)},
		{Name: "service_status", Arguments: []byte(`{"service":"nginx;touch bad"}`)},
		{Name: "service_status", Arguments: []byte(`{"service":"a/b"}`)},
		{Name: "service_status", Arguments: []byte(`{"service":"$(id)"}`)},
		{Name: "read_logs", Arguments: []byte(`{"journal":true}`)},
	}
	for _, call := range cases {
		if _, err := r.Prepare(call, false, false); err == nil {
			t.Errorf("accepted invalid %s %s", call.Name, call.Arguments)
		}
	}
	for _, name := range []string{"nginx", "nginx.service", "worker@one.service", "a-b_c.timer"} {
		if _, err := r.Prepare(toolCall(t, "service_status", map[string]any{"service": name}), false, false); err != nil {
			t.Errorf("safe service %s: %v", name, err)
		}
	}
}

func TestWriteApprovalAndVersioning(t *testing.T) {
	r, _, dir := newTestRunner(t)
	path := filepath.Join(dir, "note.txt")
	writeFixture(t, path, "旧内容\n")
	call := toolCall(t, "write_file", map[string]any{"path": "note.txt", "content": "新内容\n"})
	p, err := r.Prepare(call, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.NeedsApproval || !strings.Contains(p.Preview, hashBytes([]byte("旧内容\n"))) || !strings.Contains(p.Preview, "-旧内容") || !strings.Contains(p.Preview, "+新内容") {
		t.Fatalf("incomplete approval: %s", p.Preview)
	}
	if got := readFixture(t, path); got != "旧内容\n" {
		t.Fatal("prepare mutated file")
	}
	if _, err := r.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := readFixture(t, path); got != "新内容\n" {
		t.Fatalf("unexpected write: %q", got)
	}
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("replay allowed: %v", err)
	}

	p, err = r.Prepare(toolCall(t, "write_file", map[string]any{"path": path, "content": "approved"}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, "concurrent update")
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("changed file allowed: %v", err)
	}
	if readFixture(t, path) != "concurrent update" {
		t.Fatal("unapproved version overwritten")
	}
}

func TestCreateAndTargetAppearing(t *testing.T) {
	r, _, dir := newTestRunner(t)
	p, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": "new.txt", "content": "new"}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Preview, "ABSENT") {
		t.Fatal("missing new-file marker")
	}
	if _, err := r.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if readFixture(t, filepath.Join(dir, "new.txt")) != "new" {
		t.Fatal("not created")
	}
	p, err = r.Prepare(toolCall(t, "write_file", map[string]any{"path": "appears.txt", "content": "approved"}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "appears.txt")
	writeFixture(t, path, "other")
	if _, err := r.Execute(context.Background(), p); err == nil {
		t.Fatal("overwrote target created after approval")
	}
	if readFixture(t, path) != "other" {
		t.Fatal("existing target overwritten")
	}
}

func TestFreshPermissionsAndSealedApproval(t *testing.T) {
	r, cfg, dir := newTestRunner(t)
	path := filepath.Join(dir, "note.txt")
	writeFixture(t, path, "before")
	prepare := func() *Prepared {
		p, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": path, "content": "after"}), false, false)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := prepare()
	cfg.WriteRoots = nil
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("revoked write: %v", err)
	}
	cfg.WriteRoots = []string{dir}
	p = prepare()
	p.NeedsApproval = false
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("approval bit tampered: %v", err)
	}
	p = prepare()
	p.Call.Arguments = []byte(`{"path":"note.txt","content":"not approved"}`)
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("args tampered: %v", err)
	}
	p = prepare()
	p.Preview = "hidden"
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("preview tampered: %v", err)
	}
	p = prepare()
	other := New(func() config.Config { return *cfg }, nil)
	if _, err := other.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("foreign runner accepted: %v", err)
	}
	if _, err := r.Execute(context.Background(), &Prepared{Call: p.Call}); !errors.Is(err, ErrStale) {
		t.Fatal("forged capability accepted")
	}
	if readFixture(t, path) != "before" {
		t.Fatal("denied operation changed file")
	}
	read, err := r.Prepare(toolCall(t, "read_file", map[string]any{"path": path}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReadRoots = nil
	if _, err := r.Execute(context.Background(), read); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked read: %v", err)
	}
}

func TestDryRunAndCancellation(t *testing.T) {
	r, _, dir := newTestRunner(t)
	writeFixture(t, filepath.Join(dir, "note.txt"), "x")
	for _, call := range []Call{
		toolCall(t, "write_file", map[string]any{"path": "note.txt", "content": "y"}),
		toolCall(t, "replace_text", map[string]any{"path": "note.txt", "old_text": "x", "new_text": "y"}),
		toolCall(t, "exec", map[string]any{"command": "printf test"}),
	} {
		if _, err := r.Prepare(call, false, true); !errors.Is(err, ErrDenied) {
			t.Fatalf("dry-run mutation %s: %v", call.Name, err)
		}
	}
	p, err := r.Prepare(toolCall(t, "read_file", map[string]any{"path": "note.txt"}), false, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Execute(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if runtime.GOOS != "linux" {
		if _, err := r.Prepare(toolCall(t, "exec", map[string]any{"command": "noop"}), false, false); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("platform should reject exec: %v", err)
		}
	}
}

func TestDeniedNamesAndPaths(t *testing.T) {
	r, cfg, dir := newTestRunner(t)
	protected := filepath.Join(dir, "data")
	if err := os.Mkdir(protected, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(protected, "state.db"), "private")
	r = New(func() config.Config { return *cfg }, []string{protected})
	outside := t.TempDir()
	writeFixture(t, filepath.Join(outside, "other.txt"), "outside")
	paths := []string{".env", ".env.local", ".ssh/id_rsa", ".aws/config", "credentials.json", "server.key", "private.pem", "../other.txt", filepath.Join(protected, "state.db"), filepath.Join(outside, "other.txt")}
	if runtime.GOOS == "windows" {
		paths = append(paths, "note.txt:stream", "note.txt.", `\\server\share\file`, `C:relative.txt`)
	}
	for _, path := range paths {
		for _, name := range []string{"read_file", "write_file"} {
			args := map[string]any{"path": path}
			if name == "write_file" {
				args["content"] = "denied"
			}
			if _, err := r.Prepare(toolCall(t, name, args), false, false); !errors.Is(err, ErrDenied) {
				t.Errorf("%s %q: expected denial, got %v", name, path, err)
			}
		}
	}
	writeFixture(t, filepath.Join(dir, "visible.txt"), "okay")
	writeFixture(t, filepath.Join(dir, ".env"), "sensitive")
	res := runTool(t, r, "list_directory", map[string]any{"path": "."})
	if strings.Contains(res.Output, "data") || strings.Contains(res.Output, ".env") || !strings.Contains(res.Output, "visible.txt") {
		t.Fatalf("list leaked hidden entries: %s", res.Output)
	}
	cfg.WriteRoots = []string{outside}
	if _, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": filepath.Join(outside, "other.txt"), "content": "x"}), false, false); !errors.Is(err, ErrDenied) {
		t.Fatal("write without read permission exposed diff")
	}
}

func TestHardLinkRejection(t *testing.T) {
	r, _, dir := newTestRunner(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "source.txt")
	writeFixture(t, target, "outside contents")
	if err := os.Link(target, filepath.Join(dir, "hard.txt")); err != nil {
		t.Skipf("hard link unavailable: %v", err)
	}
	for _, name := range []string{"read_file", "write_file"} {
		args := map[string]any{"path": "hard.txt"}
		if name == "write_file" {
			args["content"] = "denied"
		}
		if _, err := r.Prepare(toolCall(t, name, args), false, false); !errors.Is(err, ErrDenied) {
			t.Fatalf("hard link %s allowed: %v", name, err)
		}
	}
	result := runTool(t, r, "list_directory", map[string]any{})
	if strings.Contains(result.Output, "hard.txt") {
		t.Fatal("hard link metadata disclosed")
	}
}

func TestSymlinkRejection(t *testing.T) {
	r, _, dir := newTestRunner(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "source.txt")
	writeFixture(t, target, "outside contents")
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, path := range []string{"linked/source.txt", "linked"} {
		name := "read_file"
		if path == "linked" {
			name = "list_directory"
		}
		if _, err := r.Prepare(toolCall(t, name, map[string]any{"path": path}), false, false); err == nil {
			t.Fatalf("symlink path allowed: %s", path)
		}
	}
	writeFixture(t, filepath.Join(dir, "plain.txt"), "before")
	p, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": "plain.txt", "content": "after"}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "plain.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "plain.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), p); err == nil {
		t.Fatal("write through replacement symlink allowed")
	}
	if readFixture(t, target) != "outside contents" {
		t.Fatal("symlink target modified")
	}
}

func TestRangesSearchAndReplacement(t *testing.T) {
	r, _, dir := newTestRunner(t)
	writeFixture(t, filepath.Join(dir, "中文.log"), "第一行\n第二行 错误\n第三行\n第四行 错误\n")
	res := runTool(t, r, "read_file", map[string]any{"path": "中文.log", "start_line": 2, "end_line": 3})
	if !strings.Contains(res.Output, "2\t第二行") || strings.Contains(res.Output, "第一行") || strings.Contains(res.Output, "第四行") {
		t.Fatalf("range: %s", res.Output)
	}
	res = runTool(t, r, "read_logs", map[string]any{"path": "中文.log", "lines": 1})
	if !strings.Contains(res.Output, "第四行") || strings.Contains(res.Output, "第三行") {
		t.Fatalf("tail: %s", res.Output)
	}
	res = runTool(t, r, "search_files", map[string]any{"query": "错误"})
	if strings.Count(res.Output, "中文.log:") != 2 {
		t.Fatalf("unicode search: %s", res.Output)
	}
	if _, err := r.Prepare(toolCall(t, "replace_text", map[string]any{"path": "中文.log", "old_text": "错误", "new_text": "正常"}), false, false); err == nil {
		t.Fatal("ambiguous replacement allowed")
	}
	runTool(t, r, "replace_text", map[string]any{"path": "中文.log", "old_text": "错误", "new_text": "正常", "all": true})
	if strings.Count(readFixture(t, filepath.Join(dir, "中文.log")), "正常") != 2 {
		t.Fatal("replace all failed")
	}
}

func TestResourceCaps(t *testing.T) {
	r, cfg, dir := newTestRunner(t)
	cfg.Limits.OutputBytes = 1024
	writeFixture(t, filepath.Join(dir, "long.txt"), strings.Repeat("中文", 2000))
	res := runTool(t, r, "read_file", map[string]any{"path": "long.txt"})
	if !res.Truncated || len(res.Output) > 1024 || !utf8.ValidString(res.Output) {
		t.Fatalf("bad output bounds: %d/%v", len(res.Output), res.Truncated)
	}
	writeFixture(t, filepath.Join(dir, "huge.txt"), strings.Repeat("x", MaxFileBytes+1))
	if _, err := r.Prepare(toolCall(t, "read_file", map[string]any{"path": "huge.txt"}), false, false); err == nil {
		t.Fatal("oversized input accepted")
	}
	if _, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": "new.txt", "content": strings.Repeat("x", MaxPreviewBytes+1)}), false, false); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("truncated approval preview permitted: %v", err)
	}
	if _, err := r.Prepare(toolCall(t, "read_file", map[string]any{"path": "long.txt", "start_line": 1, "end_line": MaxReadLines + 1}), false, false); err == nil {
		t.Fatal("oversized line range allowed")
	}
	buf := &limitedBuffer{limit: 8}
	if n, err := buf.Write([]byte(strings.Repeat("x", 32))); err != nil || n != 32 {
		t.Fatal("pipe buffer did not drain")
	}
	if result := buf.result(); len(result.Output) != 8 || !result.Truncated {
		t.Fatal("pipe buffer exceeded bounds")
	}
}
