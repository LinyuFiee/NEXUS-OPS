package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

func lineRange(a arguments) (int, int) {
	start := defaultInt(a.StartLine, 1)
	return start, defaultInt(a.EndLine, start+199)
}

func readLines(b []byte, a arguments, tail bool, limit int) Result {
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start, end := lineRange(a)
	if tail {
		end = len(lines)
		start = max(1, end-defaultInt(a.Lines, 200)+1)
	}
	var out strings.Builder
	for i := max(0, start-1); i < len(lines) && i < end; i++ {
		fmt.Fprintf(&out, "%d\t%s\n", i+1, lines[i])
		if out.Len() > limit {
			break
		}
	}
	return boundedResult(out.String(), limit, end < len(lines) && !tail)
}

func editPreview(path string, old, updated []byte, original fileVersion) (string, error) {
	oldHash := original.hash
	if !original.exists {
		oldHash = "ABSENT (new file)"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "FILE WRITE APPROVAL\nPath: %s\nOriginal SHA-256: %s\nNew SHA-256: %s\nBytes: %d -> %d\n--- original\n+++ proposed\n", path, oldHash, hashBytes(updated), len(old), len(updated))
	// One unified hunk, trimmed at common complete lines. Every changed byte is
	// displayed; changes too large for a complete approval preview are refused.
	a, b := strings.SplitAfter(string(old), "\n"), strings.SplitAfter(string(updated), "\n")
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	aEnd, bEnd := len(a), len(b)
	for aEnd > start && bEnd > start && a[aEnd-1] == b[bEnd-1] {
		aEnd--
		bEnd--
	}
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", start+1, aEnd-start, start+1, bEnd-start)
	for _, side := range []struct {
		prefix string
		lines  []string
	}{{"-", a[start:aEnd]}, {"+", b[start:bEnd]}} {
		for _, line := range side.lines {
			out.WriteString(side.prefix)
			for _, ch := range line {
				if ch == '\n' || ch == '\t' || (!unicode.IsControl(ch) && !unicode.Is(unicode.Cf, ch)) {
					out.WriteRune(ch)
				} else {
					fmt.Fprintf(&out, "\\u%04x", ch)
				}
				if out.Len() > MaxPreviewBytes {
					return "", fmt.Errorf("%w: complete diff exceeds 64 KiB; split the edit", ErrTooLarge)
				}
			}
			if !strings.HasSuffix(line, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
			if out.Len() > MaxPreviewBytes {
				return "", fmt.Errorf("%w: complete diff exceeds 64 KiB; split the edit", ErrTooLarge)
			}
		}
	}
	return out.String(), nil
}

func (p filePolicy) listDirectory(ctx context.Context, path string, count, limit int) (Result, error) {
	f, err := p.openDirectory(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	entries, err := f.ReadDir(MaxWalkEntries + 1)
	if err != nil && err != io.EOF {
		return Result{}, err
	}
	truncated := len(entries) > MaxWalkEntries
	if truncated {
		entries = entries[:MaxWalkEntries]
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out strings.Builder
	shown := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		child := filepath.Join(path, entry.Name())
		if p.check(child, false) != nil || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) || p.protectedIdentity(info) {
			continue
		}
		if !info.IsDir() {
			// Verify hard-link count and final-handle policy even for metadata-only
			// listings, so a protected file cannot be advertised via a hard link.
			parent, file, _, err := p.openFile(child, false, false)
			if err != nil {
				continue
			}
			file.Close()
			parent.Close()
		}
		if shown == count {
			truncated = true
			break
		}
		kind := "file"
		if info.IsDir() {
			kind = "directory"
		}
		fmt.Fprintf(&out, "%s\t%d\t%q\n", kind, info.Size(), entry.Name())
		shown++
		if out.Len() > limit {
			truncated = true
			break
		}
	}
	return boundedResult(out.String(), limit, truncated), nil
}

func (p filePolicy) searchFiles(ctx context.Context, path string, a arguments, limit int) (Result, error) {
	type node struct {
		path  string
		depth int
	}
	queue := []node{{path, 0}}
	seen, matches := 0, 0
	budget := &scanBudget{remaining: MaxSearchBytes}
	ctx = context.WithValue(ctx, scanBudgetKey{}, budget)
	maxResults := defaultInt(a.MaxResults, 50)
	needle := a.Query
	if !a.CaseSensitive {
		needle = strings.ToLower(needle)
	}
	var out strings.Builder
	truncated := false
search:
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		n := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		f, err := p.openDirectory(n.path)
		if err != nil {
			if n.path == path {
				return Result{}, err
			}
			continue
		}
		entries, readErr := f.ReadDir(MaxWalkEntries - seen + 1)
		f.Close()
		if readErr != nil && readErr != io.EOF {
			continue
		}
		for _, entry := range entries {
			seen++
			if seen > MaxWalkEntries {
				truncated = true
				break search
			}
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			child := filepath.Join(n.path, entry.Name())
			if p.check(child, false) != nil || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			if entry.IsDir() {
				if n.depth >= MaxSearchDepth {
					truncated = true
					continue
				}
				queue = append(queue, node{child, n.depth + 1})
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
				continue
			}
			if info.Size() > int64(budget.remaining) {
				truncated = true
				break search
			}
			b, _, err := p.readFile(ctx, child, false, false)
			if err != nil {
				if ctx.Err() != nil {
					return Result{}, ctx.Err()
				}
				if budget.exhausted {
					truncated = true
					break search
				}
				continue
			}
			if !validText(b) {
				continue
			}
			scanner := bufio.NewScanner(bytes.NewReader(b))
			scanner.Buffer(make([]byte, 4096), MaxFileBytes+1)
			lineNumber := 0
			for scanner.Scan() {
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
				lineNumber++
				line := scanner.Text()
				haystack := line
				if !a.CaseSensitive {
					haystack = strings.ToLower(haystack)
				}
				if !strings.Contains(haystack, needle) {
					continue
				}
				rel, _ := filepath.Rel(path, child)
				fmt.Fprintf(&out, "%s:%d:%s\n", filepath.ToSlash(rel), lineNumber, line)
				matches++
				if matches >= maxResults || out.Len() >= limit {
					truncated = true
					break search
				}
			}
		}
	}
	return boundedResult(out.String(), limit, truncated), nil
}
