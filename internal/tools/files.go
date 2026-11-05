package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"opsagent/internal/config"
)

type filePolicy struct {
	runner *Runner
	cfg    config.Config
}

func (r *Runner) policy(c config.Config) filePolicy { return filePolicy{runner: r, cfg: c} }

func (p filePolicy) key() string {
	b, _ := json.Marshal(struct {
		WorkDir                string
		Read, Write, Protected []string
		Timeout, Output        int
	}{p.cfg.WorkDir, p.cfg.ReadRoots, p.cfg.WriteRoots, p.runner.protected, commandSeconds(p.cfg), outputLimit(p.cfg)})
	return hashBytes(b)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func within(root, path string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	if runtime.GOOS == "windows" {
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func inRoots(path string, roots []string) bool {
	for _, root := range roots {
		if filepath.Clean(root) == filepath.VolumeName(root)+string(filepath.Separator) {
			continue
		}
		if within(root, path) {
			return true
		}
	}
	return false
}

// This conservative filename guard is NOT a content-secret detector or sandbox.
func sensitiveComponent(name string) bool {
	n := strings.TrimRight(strings.ToLower(name), " .")
	if n == ".env" || strings.HasPrefix(n, ".env.") || strings.HasSuffix(n, ".env") ||
		strings.HasSuffix(n, ".key") || strings.HasSuffix(n, ".pem") || strings.HasSuffix(n, ".p12") ||
		strings.HasSuffix(n, ".pfx") || strings.HasSuffix(n, ".ppk") || strings.HasSuffix(n, ".keystore") ||
		strings.Contains(n, "credential") || strings.Contains(n, "secret") ||
		strings.HasPrefix(n, "id_rsa") || strings.HasPrefix(n, "id_ed25519") || strings.HasPrefix(n, "id_ecdsa") || strings.HasPrefix(n, "id_dsa") ||
		strings.HasSuffix(n, "_history") {
		return true
	}
	switch n {
	case ".ssh", ".aws", ".azure", ".gcloud", "gcloud", ".config", ".local", ".kube", ".docker", ".gnupg", ".git", ".zcode",
		".npmrc", ".pypirc", ".netrc", "_netrc", ".pgpass", ".my.cnf", ".git-credentials", ".vault-token",
		"authorized_keys", "known_hosts", "kubeconfig", "shadow", "gshadow", "passwd", "sam", "security",
		"login data", "cookies", "keychain", "keychains", "wallet.dat":
		return true
	}
	return false
}

func validatePathSpelling(path string) error {
	if path == "" || len(path) > 4096 || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%w: invalid path", ErrDenied)
	}
	if runtime.GOOS == "windows" && strings.HasPrefix(path, `\\`) {
		return fmt.Errorf("%w: UNC/device paths are not supported", ErrDenied)
	}
	tail := strings.TrimPrefix(path, filepath.VolumeName(path))
	for _, c := range strings.FieldsFunc(tail, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c == ".." {
			return fmt.Errorf("%w: parent traversal is not allowed", ErrDenied)
		}
		if c == "." {
			continue
		}
		if sensitiveComponent(c) {
			return fmt.Errorf("%w: credential or private path", ErrDenied)
		}
		for _, ch := range c {
			if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) {
				return fmt.Errorf("%w: control characters in path", ErrDenied)
			}
		}
		if runtime.GOOS == "windows" && (strings.Contains(c, ":") || strings.TrimRight(c, " .") != c) {
			return fmt.Errorf("%w: ambiguous path spelling", ErrDenied)
		}
	}
	return nil
}

func (p filePolicy) absolute(path string) (string, error) {
	if err := validatePathSpelling(path); err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		if !filepath.IsAbs(p.cfg.WorkDir) || filepath.VolumeName(path) != "" {
			return "", fmt.Errorf("%w: invalid work_dir or drive-relative path", ErrDenied)
		}
		path = filepath.Join(p.cfg.WorkDir, path)
	}
	path = filepath.Clean(path)
	if err := validatePathSpelling(path); err != nil {
		return "", err
	}
	return path, nil
}

func (p filePolicy) protectedPath(path string) bool {
	for _, root := range p.runner.protected {
		if within(root, path) {
			return true
		}
	}
	return false
}

func (p filePolicy) check(path string, write bool) error {
	if p.runner.invalidProtect || !filepath.IsAbs(path) || p.protectedPath(path) {
		return fmt.Errorf("%w: protected path", ErrDenied)
	}
	if err := validatePathSpelling(path); err != nil {
		return err
	}
	if !inRoots(path, p.cfg.ReadRoots) || (write && !inRoots(path, p.cfg.WriteRoots)) {
		return fmt.Errorf("%w: path is outside current authorized roots", ErrDenied)
	}
	return nil
}

func (p filePolicy) protectedIdentity(fi os.FileInfo) bool {
	if fi == nil {
		return true
	}
	for _, protected := range p.runner.protectedInfo {
		if os.SameFile(fi, protected) {
			return true
		}
	}
	for _, path := range p.runner.protected {
		if protected, err := os.Stat(path); err == nil && os.SameFile(fi, protected) {
			return true
		}
	}
	return false
}

// Pin each directory using os.Root and verify identity on both sides of open.
// All symlinks (including configured roots) are rejected. EvalSymlinks is never
// used to authorize a path that is then reopened by ambient absolute pathname.
// Final handles are checked as well. Hostile mount changes or privileged rename
// of an already-open directory require OS isolation, not string path checks.
func (p filePolicy) pinnedDirectory(path string) (*os.Root, error) {
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	rel := strings.TrimPrefix(filepath.Clean(path), volume)
	if rel == "" {
		return root, nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		before, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || p.protectedIdentity(before) {
			root.Close()
			return nil, fmt.Errorf("%w: linked, protected or non-directory path component", ErrDenied)
		}
		next, err := root.OpenRoot(part)
		root.Close()
		if err != nil {
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) || p.protectedIdentity(after) {
			next.Close()
			return nil, fmt.Errorf("%w: directory changed while opening", ErrDenied)
		}
		root = next
	}
	return root, nil
}

func (p filePolicy) validateHandle(f *os.File, expected string, write bool) error {
	actual, err := openedPath(f)
	if err != nil {
		return fmt.Errorf("%w: cannot verify opened file path: %v", ErrDenied, err)
	}
	if !samePath(actual, expected) {
		return fmt.Errorf("%w: path changed or resolved through an alias", ErrDenied)
	}
	return p.check(actual, write)
}

func (p filePolicy) openDirectory(path string) (*os.File, error) {
	if err := p.check(path, false); err != nil {
		return nil, err
	}
	root, err := p.pinnedDirectory(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	if err := p.validateHandle(f, path, false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

type fileVersion struct {
	exists bool
	hash   string
	info   os.FileInfo
	parent os.FileInfo
}

func (v fileVersion) equal(other fileVersion) bool {
	if v.exists != other.exists || v.parent == nil || other.parent == nil || !os.SameFile(v.parent, other.parent) {
		return false
	}
	if !v.exists {
		return true
	}
	return v.hash == other.hash && v.info != nil && other.info != nil && os.SameFile(v.info, other.info) && v.info.Mode() == other.info.Mode()
}

func (p filePolicy) openFile(path string, write bool, allowMissing bool) (*os.Root, *os.File, fileVersion, error) {
	var v fileVersion
	if err := p.check(path, write); err != nil {
		return nil, nil, v, err
	}
	parent, err := p.pinnedDirectory(filepath.Dir(path))
	if err != nil {
		return nil, nil, v, err
	}
	fail := func(err error) (*os.Root, *os.File, fileVersion, error) { parent.Close(); return nil, nil, v, err }
	v.parent, err = parent.Stat(".")
	if err != nil {
		return fail(err)
	}
	dir, err := parent.Open(".")
	if err != nil {
		return fail(err)
	}
	actual, pathErr := openedPath(dir)
	dir.Close()
	if pathErr != nil || !samePath(actual, filepath.Dir(path)) || p.protectedPath(actual) {
		return fail(fmt.Errorf("%w: parent changed while opening", ErrDenied))
	}
	name := filepath.Base(path)
	before, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		return parent, nil, v, nil
	}
	if err != nil {
		return fail(err)
	}
	if !before.Mode().IsRegular() || before.Size() > MaxFileBytes {
		return fail(fmt.Errorf("%w: only regular files up to 1 MiB are allowed", ErrDenied))
	}
	flags := os.O_RDONLY
	if write {
		flags = os.O_RDWR
	}
	f, err := parent.OpenFile(name, safeOpenFlags(flags), 0)
	if err != nil {
		return fail(err)
	}
	after, err := f.Stat()
	if err == nil && (!after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() > MaxFileBytes) {
		err = fmt.Errorf("%w: file changed while opening", ErrDenied)
	}
	if err == nil {
		err = singleLink(f, after)
	}
	if err == nil {
		err = p.validateHandle(f, path, write)
	}
	if err != nil {
		f.Close()
		return fail(err)
	}
	v.exists, v.info = true, after
	return parent, f, v, nil
}

type scanBudgetKey struct{}
type scanBudget struct {
	remaining int
	exhausted bool
}

func readBounded(ctx context.Context, f *os.File) ([]byte, error) {
	var b bytes.Buffer
	chunk := make([]byte, 32768)
	budget, _ := ctx.Value(scanBudgetKey{}).(*scanBudget)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		readSize := len(chunk)
		if budget != nil {
			if budget.remaining == 0 {
				info, err := f.Stat()
				if err == nil && info.Size() == int64(b.Len()) {
					return b.Bytes(), nil
				}
				budget.exhausted = true
				return nil, ErrTooLarge
			}
			readSize = min(readSize, budget.remaining)
		}
		n, err := f.Read(chunk[:readSize])
		if budget != nil {
			budget.remaining -= n
		}
		if n > 0 {
			if b.Len()+n > MaxFileBytes {
				return nil, ErrTooLarge
			}
			b.Write(chunk[:n])
		}
		if err == io.EOF {
			return b.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (p filePolicy) readFile(ctx context.Context, path string, write, allowMissing bool) ([]byte, fileVersion, error) {
	parent, f, v, err := p.openFile(path, write, allowMissing)
	if err != nil {
		return nil, v, err
	}
	defer parent.Close()
	if f == nil {
		return nil, v, nil
	}
	defer f.Close()
	b, err := readBounded(ctx, f)
	if err != nil {
		return nil, v, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, v, err
	}
	if v.info.Size() != after.Size() || !v.info.ModTime().Equal(after.ModTime()) {
		return nil, v, ErrStale
	}
	if err := p.validateHandle(f, path, write); err != nil {
		return nil, v, err
	}
	v.hash = hashBytes(b)
	return b, v, nil
}

func (p filePolicy) writeFile(ctx context.Context, path string, original fileVersion, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, f, current, err := p.openFile(path, true, !original.exists)
	if err != nil {
		return err
	}
	defer parent.Close()
	if current.exists {
		defer f.Close()
		unlock, err := lockFile(f)
		if err != nil {
			return err
		}
		defer unlock()
		old, err := readBounded(ctx, f)
		if err != nil {
			return err
		}
		current.hash = hashBytes(old)
		if !original.equal(current) {
			return ErrStale
		}
		if err := p.validateHandle(f, path, true); err != nil {
			return err
		}
		entry, err := parent.Lstat(filepath.Base(path))
		if err != nil || !os.SameFile(entry, current.info) || !entry.Mode().IsRegular() {
			return ErrStale
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	} else {
		if !original.equal(current) {
			return ErrStale
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.runner.policy(p.runner.configuration()).key() != p.key() {
			return fmt.Errorf("%w: permissions changed before creating file", ErrStale)
		}
		f, err = parent.OpenFile(filepath.Base(path), safeOpenFlags(os.O_CREATE|os.O_EXCL|os.O_RDWR), 0600)
		if err != nil {
			return fmt.Errorf("%w: destination appeared or cannot be created", ErrStale)
		}
		defer f.Close()
		if err := p.validateHandle(f, path, true); err != nil {
			return err
		}
	}
	if err := p.validateHandle(f, path, true); err != nil {
		return err
	}
	// This additional fresh-config check is immediately before the first write;
	// getConfig has snapshot semantics (callers must publish immutable snapshots).
	if p.runner.policy(p.runner.configuration()).key() != p.key() {
		return fmt.Errorf("%w: permissions changed during verification", ErrStale)
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrStale
	}
	if err := singleLink(f, info); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The checked handle is never reopened/truncated by pathname. In-place write
	// preserves mode/ownership. Locks protect cooperative writers; hostile same-
	// inode writers and crash-atomic transactions require OS/filesystem controls.
	if _, err := f.Write(content); err != nil {
		return err
	}
	if err := f.Truncate(int64(len(content))); err != nil {
		return err
	}
	return f.Sync()
}
