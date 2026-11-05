//go:build !linux

package tools

import (
	"context"
	"os"
)
func shellSupported()bool{return false}
func executeShell(ctx context.Context,command string,cwd *os.File,seconds,limit int)(Result,error){return Result{},ErrUnsupported}
func serviceStatus(ctx context.Context,service string,seconds,limit int)(Result,error){return Result{},ErrUnsupported}
