package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opsagent/internal/config"
)

func TestSchemaAndPreviewIntegrity(t *testing.T) {
	r, _, dir := newTestRunner(t)
	for _, d := range r.Definitions(false) {
		b, err := json.Marshal(d.Parameters)
		if err != nil || strings.Contains(string(b), `"required":null`) {
			t.Fatalf("invalid tool schema %s: %s (%v)", d.Name, b, err)
		}
	}
	writeFixture(t, filepath.Join(dir, "normal.txt"), "before\n")
	p, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": "normal.txt", "content": "\x1b[2J\u202eafter\n"}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(p.Preview, '\x1b') || strings.ContainsRune(p.Preview, '\u202e') || !strings.Contains(p.Preview, `\u001b`) || !strings.Contains(p.Preview, `\u202e`) {
		t.Fatalf("unsafe preview: %q", p.Preview)
	}
	for _, path := range []string{"a\x1b.txt", "a\u202e.txt"} {
		if _, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": path, "content": "x"}), false, false); !errors.Is(err, ErrDenied) {
			t.Fatalf("control filename accepted: %v", err)
		}
	}
}

func TestReadAndDirectoryIdentityChanges(t *testing.T) {
	r, _, dir := newTestRunner(t)
	path := filepath.Join(dir, "read.txt")
	writeFixture(t, path, "before")
	read, err := r.Prepare(toolCall(t, "read_file", map[string]any{"path": path}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, "after")
	if _, err := r.Execute(context.Background(), read); !errors.Is(err, ErrStale) {
		t.Fatalf("stale read accepted: %v", err)
	}
	parent := filepath.Join(dir, "parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(parent, "note.txt"), "before")
	p, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": filepath.Join(parent, "note.txt"), "content": "after"}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, filepath.Join(dir, "renamed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(parent, "note.txt"), "before")
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("replaced parent accepted: %v", err)
	}
	if readFixture(t, filepath.Join(parent, "note.txt")) != "before" {
		t.Fatal("replacement file changed")
	}
}

func TestFreshConfigImmediatelyBeforeWrite(t *testing.T) {
	_, cfg, dir := newTestRunner(t)
	writeFixture(t, filepath.Join(dir, "note.txt"), "before")
	calls := 0
	r := New(func() config.Config {
		calls++
		c := *cfg
		if calls >= 3 {
			c.WriteRoots = nil
		}
		return c
	}, nil)
	p, err := r.Prepare(toolCall(t, "write_file", map[string]any{"path": "note.txt", "content": "after"}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), p); !errors.Is(err, ErrStale) {
		t.Fatalf("mid-verification permission change accepted: %v", err)
	}
	if readFixture(t, filepath.Join(dir, "note.txt")) != "before" {
		t.Fatal("revoked write changed file")
	}
}

func TestSearchBudgetAndDepth(t *testing.T) {
	r, _, dir := newTestRunner(t)
	large := strings.Repeat("a", MaxFileBytes)
	for i := 0; i < MaxSearchBytes/MaxFileBytes+1; i++ {
		writeFixture(t, filepath.Join(dir, fmt.Sprintf("file-%02d.txt", i)), large)
	}
	result := runTool(t, r, "search_files", map[string]any{"query": "missing"})
	if !result.Truncated || result.Output != "" {
		t.Fatalf("scan budget not enforced: %+v", result)
	}
	depthRoot := filepath.Join(dir, "deep")
	current := depthRoot
	for i := 0; i < MaxSearchDepth+2; i++ {
		if err := os.Mkdir(current, 0700); err != nil {
			t.Fatal(err)
		}
		current = filepath.Join(current, "d")
	}
	writeFixture(t, filepath.Join(filepath.Dir(current), "hidden.txt"), "needle")
	result = runTool(t, r, "search_files", map[string]any{"path": depthRoot, "query": "needle"})
	if !result.Truncated || result.Output != "" {
		t.Fatalf("depth budget not enforced: %+v", result)
	}
}

func TestReadBudgetCountsPartialReads(t *testing.T) {
	_, _, dir := newTestRunner(t)
	path := filepath.Join(dir, "data.txt")
	writeFixture(t, path, "123456789")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	budget := &scanBudget{remaining: 3}
	ctx := context.WithValue(context.Background(), scanBudgetKey{}, budget)
	if _, err := readBounded(ctx, f); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("budget: %v", err)
	}
	if budget.remaining != 0 || !budget.exhausted {
		t.Fatal("partial read was not charged")
	}
}

func TestInvalidProtectedConfigurationFailsClosed(t *testing.T) {
	_, cfg, dir := newTestRunner(t)
	writeFixture(t, filepath.Join(dir, "note.txt"), "x")
	r := New(func() config.Config { return *cfg }, []string{"relative-data"})
	if _, err := r.Prepare(toolCall(t, "read_file", map[string]any{"path": "note.txt"}), false, false); !errors.Is(err, ErrDenied) {
		t.Fatal("invalid protected path opened")
	}
}
