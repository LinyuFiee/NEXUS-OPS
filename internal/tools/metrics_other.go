//go:build !linux

package tools

import (
	"context"
	"encoding/json"
	"os"
)

func Sample(ctx context.Context,workDir string)(Metrics,error){m:=baseMetrics();m.Unsupported=[]string{"cpu_percent","memory_percent","disk_percent","uptime_seconds","process_rss","system_process_list"};if e:=ctx.Err();e!=nil{return m,e};return m,nil}
func listProcesses(ctx context.Context,count,limit int)(Result,error){if e:=ctx.Err();e!=nil{return Result{},e};b,_:=json.Marshal(map[string]any{"processes":[]map[string]any{{"pid":os.Getpid(),"name":"ops-agent"}},"unsupported":[]string{"system_process_list"}});return boundedResult(string(b),limit,false),nil}
