//go:build linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func shellSupported()bool{return true}
func executeShell(ctx context.Context,command string,cwd *os.File,seconds,limit int)(Result,error){
	if cwd==nil{return Result{},ErrDenied}
	cmd:=exec.Command("/bin/sh","-c",command)
	// Resolve the held parent descriptor, independent of child fd remapping order.
	cmd.Dir=fmt.Sprintf("/proc/%d/fd/%d",os.Getpid(),cwd.Fd())
	return runProcess(ctx,cmd,seconds,limit)
}
func serviceStatus(ctx context.Context,service string,seconds,limit int)(Result,error){
	if !serviceName.MatchString(service){return Result{},ErrInvalidCall}
	bin:="/usr/bin/systemctl";if _,e:=os.Stat(bin);e!=nil{bin="/bin/systemctl"}
	cmd:=exec.Command(bin,"show","--no-pager","--plain","--property=Id,LoadState,ActiveState,SubState,MainPID,UnitFileState","--",service)
	cmd.Dir="/";return runProcess(ctx,cmd,min(seconds,10),limit)
}
func runProcess(parent context.Context,cmd *exec.Cmd,seconds,limit int)(Result,error){
	ctx,cancel:=context.WithTimeout(parent,time.Duration(max(1,min(seconds,300)))*time.Second);defer cancel()
	if e:=ctx.Err();e!=nil{return Result{},e}
	cmd.Env=[]string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin","LANG=C.UTF-8","LC_ALL=C.UTF-8"}
	cmd.SysProcAttr=&syscall.SysProcAttr{Setpgid:true};cmd.WaitDelay=2*time.Second
	output:=&limitedBuffer{limit:min(limit,MaxOutputBytes)};cmd.Stdout=output;cmd.Stderr=output
	if e:=cmd.Start();e!=nil{return Result{},e};pid:=cmd.Process.Pid;wait:=make(chan error,1);go func(){wait<-cmd.Wait()}()
	var err error
	select{
	case err=<-wait:
		// No unmanaged jobs may survive a one-shot tool, even if the shell exited first.
		_ = syscall.Kill(-pid,syscall.SIGTERM);_ = syscall.Kill(-pid,syscall.SIGKILL)
	case<-ctx.Done():
		_=syscall.Kill(-pid,syscall.SIGTERM);timer:=time.NewTimer(350*time.Millisecond);waited:=false
		select{case<-timer.C:case err=<-wait:waited=true;if !timer.Stop(){select{case<-timer.C:default:}}}
		_=syscall.Kill(-pid,syscall.SIGKILL)
		if !waited{select{case err=<-wait:case<-time.After(3*time.Second):err=errors.New("子进程取消后未及时退出")}}
		result:=output.result();result.ExitCode=-1;return result,ctx.Err()
	}
	result:=output.result();if cmd.ProcessState!=nil{result.ExitCode=cmd.ProcessState.ExitCode()};if err!=nil{return result,fmt.Errorf("命令退出码 %d: %w",result.ExitCode,err)};return result,nil
}
