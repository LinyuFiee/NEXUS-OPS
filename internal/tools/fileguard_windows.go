//go:build windows

package tools

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	guardKernel32  = syscall.NewLazyDLL("kernel32.dll")
	guardFinalPath = guardKernel32.NewProc("GetFinalPathNameByHandleW")
	guardLock      = guardKernel32.NewProc("LockFileEx")
	guardUnlock    = guardKernel32.NewProc("UnlockFileEx")
)

func openedPath(f *os.File) (string, error) {
	buf := make([]uint16, 32768)
	n, _, callErr := guardFinalPath.Call(f.Fd(), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if n == 0 || n >= uintptr(len(buf)) {
		return "", fmt.Errorf("final file path unavailable: %v", callErr)
	}
	path := syscall.UTF16ToString(buf[:n])
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return "", ErrDenied
	}
	return strings.TrimPrefix(path, `\\?\`), nil
}

func safeOpenFlags(flags int) int { return flags }

func singleLink(f *os.File, _ os.FileInfo) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("%w: hard-linked files are not allowed", ErrDenied)
	}
	return nil
}

func lockFile(f *os.File) (func(), error) {
	overlap := new(syscall.Overlapped)
	const exclusiveAndImmediate = 0x2 | 0x1
	ok, _, _ := guardLock.Call(f.Fd(), exclusiveAndImmediate, 0, 0xffffffff, 0xffffffff, uintptr(unsafe.Pointer(overlap)))
	if ok == 0 {
		return nil, fmt.Errorf("%w: file is busy; prepare again", ErrStale)
	}
	return func() {
		_, _, _ = guardUnlock.Call(f.Fd(), 0, 0xffffffff, 0xffffffff, uintptr(unsafe.Pointer(overlap)))
	}, nil
}
