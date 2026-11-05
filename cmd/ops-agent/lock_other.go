//go:build !linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func lockDataDir(dir string) (func(), error) {
	path := filepath.Join(dir, "instance.lock")
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, errors.New("数据目录已有实例或遗留 instance.lock；确认旧开发进程已退出后再移除锁文件")
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close()
	return func() { _ = os.Remove(path) }, nil
}
