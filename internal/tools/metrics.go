package tools

import (
	"os"
	"runtime"
	"time"
)

type Metrics struct {
	OS string `json:"os"`
	Hostname string `json:"hostname"`
	CPUPercent float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	DiskPercent float64 `json:"disk_percent"`
	MemoryTotal uint64 `json:"memory_total"`
	MemoryAvailable uint64 `json:"memory_available"`
	DiskTotal uint64 `json:"disk_total"`
	DiskAvailable uint64 `json:"disk_available"`
	UptimeSeconds float64 `json:"uptime_seconds"`
	ProcessRSS uint64 `json:"process_rss"`
	GoHeapBytes uint64 `json:"go_heap_bytes"`
	Goroutines int `json:"goroutines"`
	Unsupported []string `json:"unsupported"`
	SampledAt time.Time `json:"sampled_at"`
}
func baseMetrics()Metrics{var ms runtime.MemStats;runtime.ReadMemStats(&ms);host,_:=os.Hostname();return Metrics{OS:runtime.GOOS,Hostname:host,GoHeapBytes:ms.HeapAlloc,Goroutines:runtime.NumGoroutine(),Unsupported:[]string{},SampledAt:time.Now().UTC()}}
