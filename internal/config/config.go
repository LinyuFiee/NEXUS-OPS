package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

type Model struct {
	BaseURL string `json:"base_url"`
	Name    string `json:"name"`
}

type Persona struct {
	Name         string `json:"name"`
	Preset       string `json:"preset"`
	Role         string `json:"role"`
	Address      string `json:"address"`
	Tone         string `json:"tone"`
	Detail       string `json:"detail"`
	OwnerProfile string `json:"owner_profile"`
	HostProfile  string `json:"host_profile"`
}

type Memory struct {
	Enabled    bool `json:"enabled"`
	MaxResults int  `json:"max_results"`
	MaxChars   int  `json:"max_chars"`
}

type Limits struct {
	MaxRounds       int `json:"max_rounds"`
	TaskSeconds     int `json:"task_seconds"`
	CommandSeconds  int `json:"command_seconds"`
	OutputBytes     int `json:"output_bytes"`
	ContextChars    int `json:"context_chars"`
	HistoryMessages int `json:"history_messages"`
	RetentionDays   int `json:"retention_days"`
	MaxQueue        int `json:"max_queue"`
}

type Channel struct {
	Enabled bool     `json:"enabled"`
	DryRun  bool     `json:"dry_run"`
	BotID   string   `json:"bot_id"`
	AppID   string   `json:"app_id"`
	URL     string   `json:"url"`
	Groups  []string `json:"groups"`
}

type Channels struct {
	Official Channel `json:"official"`
	OneBot   Channel `json:"onebot"`
}

type Watch struct {
	Enabled         bool    `json:"enabled"`
	IntervalSeconds int     `json:"interval_seconds"`
	MemoryPercent   float64 `json:"memory_percent"`
	DiskPercent     float64 `json:"disk_percent"`
	CPUPercent      float64 `json:"cpu_percent"`
	Consecutive     int     `json:"consecutive"`
	CooldownSeconds int     `json:"cooldown_seconds"`
	DailyLimit      int     `json:"daily_limit"`
	QuietStart      string  `json:"quiet_start"`
	QuietEnd        string  `json:"quiet_end"`
	NotifyChannel   string  `json:"notify_channel"`
	NotifyUser      string  `json:"notify_user"`
}

type Config struct {
	WorkDir    string   `json:"work_dir"`
	ReadRoots  []string `json:"read_roots"`
	WriteRoots []string `json:"write_roots"`
	Model      Model    `json:"model"`
	Persona    Persona  `json:"persona"`
	Memory     Memory   `json:"memory"`
	Limits     Limits   `json:"limits"`
	Channels   Channels `json:"channels"`
	Watch      Watch    `json:"watch"`
}

type Secrets struct {
	ModelKey       string `json:"model_key"`
	OfficialSecret string `json:"official_secret"`
	OneBotToken    string `json:"onebot_token"`
}

func Default(workDir string) Config {
	return Config{
		WorkDir: workDir, ReadRoots: []string{workDir}, WriteRoots: []string{workDir},
		Model:    Model{BaseURL: "https://api.deepseek.com", Name: "deepseek-chat"},
		Persona:  Persona{Name: "Ops 助手", Preset: "steward", Role: "单机服务器运维搭档。先诊断后操作，区分事实、推测和执行结果。", Address: "你", Tone: "沉稳、清晰、友好", Detail: "balanced"},
		Memory:   Memory{Enabled: true, MaxResults: 5, MaxChars: 1800},
		Limits:   Limits{MaxRounds: 12, TaskSeconds: 600, CommandSeconds: 60, OutputBytes: 16384, ContextChars: 48000, HistoryMessages: 24, RetentionDays: 30, MaxQueue: 8},
		Channels: Channels{Official: Channel{DryRun: true, Groups: []string{}}, OneBot: Channel{DryRun: true, Groups: []string{}}},
		Watch:    Watch{IntervalSeconds: 300, MemoryPercent: 90, DiskPercent: 90, CPUPercent: 95, Consecutive: 2, CooldownSeconds: 1800, DailyLimit: 10, QuietStart: "23:00", QuietEnd: "08:00"},
	}
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.WorkDir) {
		return errors.New("工作目录必须为绝对路径")
	}
	if len(c.ReadRoots) == 0 || len(c.ReadRoots) > 16 || len(c.WriteRoots) > 16 {
		return errors.New("需配置 1–16 个读取目录，写入目录至多 16 个")
	}
	for _, p := range append(append([]string{}, c.ReadRoots...), c.WriteRoots...) {
		if !filepath.IsAbs(p) || filepath.Clean(p) == filepath.VolumeName(p)+string(filepath.Separator) {
			return errors.New("工具目录必须为绝对路径且不能是文件系统根目录")
		}
	}
	if c.Model.BaseURL != "" {
		u, err := url.Parse(c.Model.BaseURL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("模型 API 地址无效")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
			return errors.New("远程模型地址必须使用 HTTPS；HTTP 仅限本机")
		}
	}
	if len(c.Model.Name) > 150 || len(c.Persona.Name) == 0 || len(c.Persona.Name) > 100 || len(c.Persona.Role)+len(c.Persona.Tone)+len(c.Persona.OwnerProfile)+len(c.Persona.HostProfile) > 8000 {
		return errors.New("人设或模型配置长度超限")
	}
	l := c.Limits
	if l.MaxRounds < 1 || l.MaxRounds > 30 || l.TaskSeconds < 30 || l.TaskSeconds > 1800 || l.CommandSeconds < 1 || l.CommandSeconds > 300 || l.OutputBytes < 1024 || l.OutputBytes > 65536 || l.ContextChars < 4000 || l.ContextChars > 120000 || l.HistoryMessages < 2 || l.HistoryMessages > 100 || l.RetentionDays < 1 || l.RetentionDays > 365 || l.MaxQueue < 1 || l.MaxQueue > 32 {
		return errors.New("资源限制超出允许范围")
	}
	if c.Memory.MaxResults < 1 || c.Memory.MaxResults > 10 || c.Memory.MaxChars < 100 || c.Memory.MaxChars > 6000 {
		return errors.New("记忆检索预算无效")
	}
	w := c.Watch
	if w.IntervalSeconds < 30 || w.IntervalSeconds > 86400 || w.Consecutive < 1 || w.Consecutive > 20 || w.CooldownSeconds < 60 || w.DailyLimit < 1 || w.DailyLimit > 100 {
		return errors.New("巡检周期或通知额度无效")
	}
	for _, x := range []float64{w.MemoryPercent, w.DiskPercent, w.CPUPercent} {
		if x < 1 || x > 100 {
			return errors.New("巡检阈值应在 1–100 之间")
		}
	}
	for _, s := range []string{w.QuietStart, w.QuietEnd} {
		if s != "" {
			var h, m int
			if n, _ := fmt.Sscanf(s, "%d:%d", &h, &m); n != 2 || h < 0 || h > 23 || m < 0 || m > 59 || len(s) != 5 {
				return errors.New("免打扰时间格式为 HH:MM")
			}
		}
	}
	if w.NotifyChannel != "" && w.NotifyChannel != "onebot" && w.NotifyChannel != "official" {
		return errors.New("通知渠道无效")
	}
	for name, ch := range map[string]Channel{"onebot": c.Channels.OneBot, "official": c.Channels.Official} {
		if len(ch.Groups) > 32 {
			return errors.New("群授权数量超限")
		}
		if !ch.Enabled {
			continue
		}
		if name == "onebot" {
			u, err := url.Parse(ch.URL)
			if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("OneBot 地址必须为不带凭据的 ws/wss 地址")
			}
			if strings.TrimSpace(ch.BotID) == "" {
				return errors.New("OneBot 需填写预期机器人号")
			}
		} else if strings.TrimSpace(ch.AppID) == "" {
			return errors.New("QQ 官方接入需填写 AppID")
		}
	}
	return nil
}
