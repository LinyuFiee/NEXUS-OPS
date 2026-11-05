//go:build linux

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cpuTicks()(total,idle uint64,err error){f,e:=os.Open("/proc/stat");if e!=nil{return 0,0,e};defer f.Close();sc:=bufio.NewScanner(f);if !sc.Scan(){return 0,0,fmt.Errorf("missing CPU metrics")};parts:=strings.Fields(sc.Text());if len(parts)<5||parts[0]!="cpu"{return 0,0,fmt.Errorf("invalid CPU metrics")};for i,p:=range parts[1:]{if i>=8{break};n,e:=strconv.ParseUint(p,10,64);if e!=nil{return 0,0,e};total+=n;if i==3||i==4{idle+=n}};return total,idle,nil}
func Sample(ctx context.Context,workDir string)(Metrics,error){
	m:=baseMetrics();t1,i1,e1:=cpuTicks();timer:=time.NewTimer(120*time.Millisecond);defer timer.Stop();select{case<-ctx.Done():return m,ctx.Err();case<-timer.C:}
	t2,i2,e2:=cpuTicks();if e1==nil&&e2==nil&&t2>t1&&i2>=i1{m.CPUPercent=100*(1-float64(i2-i1)/float64(t2-t1))}else{m.Unsupported=append(m.Unsupported,"cpu_percent")}
	if b,e:=os.ReadFile("/proc/meminfo");e==nil{v:=map[string]uint64{};for _,line:=range strings.Split(string(b),"\n"){p:=strings.Fields(line);if len(p)>=2{n,_:=strconv.ParseUint(p[1],10,64);v[strings.TrimSuffix(p[0],":")]=n*1024}};m.MemoryTotal=v["MemTotal"];m.MemoryAvailable=v["MemAvailable"];if m.MemoryTotal>0{m.MemoryPercent=100*(1-float64(m.MemoryAvailable)/float64(m.MemoryTotal))}else{m.Unsupported=append(m.Unsupported,"memory_percent")}}else{m.Unsupported=append(m.Unsupported,"memory_percent")}
	var fs syscall.Statfs_t;if e:=syscall.Statfs(workDir,&fs);e==nil&&fs.Blocks>0{m.DiskTotal=fs.Blocks*uint64(fs.Bsize);m.DiskAvailable=fs.Bavail*uint64(fs.Bsize);m.DiskPercent=100*(1-float64(fs.Bavail)/float64(fs.Blocks))}else{m.Unsupported=append(m.Unsupported,"disk_percent")}
	if b,e:=os.ReadFile("/proc/uptime");e==nil{p:=strings.Fields(string(b));if len(p)>0{m.UptimeSeconds,_=strconv.ParseFloat(p[0],64)}}
	if b,e:=os.ReadFile("/proc/self/statm");e==nil{p:=strings.Fields(string(b));if len(p)>1{n,_:=strconv.ParseUint(p[1],10,64);m.ProcessRSS=n*uint64(os.Getpagesize())}}
	return m,nil
}
func listProcesses(ctx context.Context,count,limit int)(Result,error){
	f,e:=os.Open("/proc");if e!=nil{return Result{},e};defer f.Close();rows:=[]map[string]any{};scanned:=0
	for len(rows)<min(count,200)&&scanned<8192{if e=ctx.Err();e!=nil{return Result{},e};ents,readErr:=f.ReadDir(128);for _,ent:=range ents{scanned++;pid,err:=strconv.Atoi(ent.Name());if err!=nil||!ent.IsDir(){continue};b,err:=os.ReadFile("/proc/"+ent.Name()+"/comm");if err!=nil{continue};name:=strings.TrimSpace(string(b));if len(name)>128{name=name[:128]};rows=append(rows,map[string]any{"pid":pid,"name":name});if len(rows)>=min(count,200){break}};if readErr!=nil{break}}
	b,_:=json.Marshal(map[string]any{"processes":rows,"limited":len(rows)>=count||scanned>=8192});return boundedResult(string(b),limit,false),nil
}
