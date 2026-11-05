//go:build linux

package tools

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func openedPath(f *os.File) (string, error) {
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(path, " (deleted)") {
		return "", ErrStale
	}
	return path, nil
}

func safeOpenFlags(flags int) int { return flags | syscall.O_NOFOLLOW | syscall.O_NONBLOCK }

func singleLink(_ *os.File, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 {
		return fmt.Errorf("%w: hard-linked files are not allowed", ErrDenied)
	}
	return nil
}

func lockFile(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("%w: file is busy; prepare again", ErrStale)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
