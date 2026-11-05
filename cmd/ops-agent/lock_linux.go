//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func lockDataDir(dir string) (func(), error) {
	f, e := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("此数据目录已有运行中的实例")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
