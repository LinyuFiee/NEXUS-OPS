//go:build linux

package tools

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestShellFixedEnvironmentAndDirectory(t *testing.T){t.Setenv("OPS_TEST_SECRET","must-not-leak");dir:=t.TempDir();f,e:=os.Open(dir);if e!=nil{t.Fatal(e)};defer f.Close();r,e:=executeShell(context.Background(),`pwd; printf '\nsecret=%s\n' "$OPS_TEST_SECRET"`,f,5,4096);if e!=nil{t.Fatal(e)};if !strings.Contains(r.Output,dir)||strings.Contains(r.Output,"must-not-leak"){t.Fatalf("unsafe env/cwd: %s",r.Output)}}
func TestShellCancellationAndOutputCap(t *testing.T){f,e:=os.Open(t.TempDir());if e!=nil{t.Fatal(e)};defer f.Close();ctx,cancel:=context.WithTimeout(context.Background(),150*time.Millisecond);defer cancel();start:=time.Now();_,e=executeShell(ctx,"sleep 20 & wait",f,30,1024);if e==nil||time.Since(start)>4*time.Second{t.Fatalf("cancel failed: %v",e)};r,e:=executeShell(context.Background(),"i=0; while [ $i -lt 500 ]; do printf '0123456789'; i=$((i+1)); done",f,5,100);if e!=nil||!r.Truncated||len(r.Output)>100{t.Fatalf("output bound failed: %+v %v",r,e)}}
func TestLinuxMetrics(t *testing.T){m,e:=Sample(context.Background(),t.TempDir());if e!=nil{t.Fatal(e)};if m.OS!="linux"||m.MemoryTotal==0||m.DiskTotal==0||m.CPUPercent<0||m.CPUPercent>100{t.Fatalf("invalid metrics %+v",m)}}
