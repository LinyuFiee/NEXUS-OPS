//go:build !linux && !windows

package tools

import "os"

// Fail closed where final-handle identity/path verification is not implemented.
// Metrics remain available with an explicit unsupported list on these platforms.
func openedPath(_ *os.File) (string, error)      { return "", ErrUnsupported }
func safeOpenFlags(flags int) int                { return flags }
func singleLink(_ *os.File, _ os.FileInfo) error { return ErrUnsupported }
func lockFile(_ *os.File) (func(), error)        { return nil, ErrUnsupported }
