package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"opsagent/internal/agent"
	"opsagent/internal/channels"
	"opsagent/internal/config"
	"opsagent/internal/memory"
	"opsagent/internal/server"
	"opsagent/internal/setup"
	"opsagent/internal/store"
	"opsagent/internal/tools"
	"opsagent/internal/watch"
)

func main() {
	if e := run(); e != nil {
		log.Print(e)
		os.Exit(1)
	}
}
func run() error {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("ops-agent "+cmd, flag.ContinueOnError)
	defaultData, _ := os.UserConfigDir()
	if defaultData == "" {
		defaultData = "."
	}
	defaultData = filepath.Join(defaultData, "ops-agent")
	data := fs.String("data-dir", defaultData, "私有配置与数据目录")
	work := fs.String("work-dir", "", "初始工作目录（必须已存在）")
	listen := fs.String("listen", "127.0.0.1:7788", "监听地址；默认仅本机")
	public := fs.String("public-url", "", "浏览器访问 origin，远程需 https://域名")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if cmd == "version" {
		fmt.Println("NEXUS / OPS", server.Version, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if *work == "" {
		cwd, e := os.Getwd()
		if e != nil {
			return e
		}
		*work = cwd
	}
	abs, e := filepath.Abs(*work)
	if e != nil {
		return e
	}
	db, e := store.Open(*data)
	if e != nil {
		return fmt.Errorf("打开数据目录失败: %w", e)
	}
	defer db.Close()
	cfg, e := config.Load(db, abs)
	if e != nil {
		return e
	}
	switch cmd {
	case "setup-code":
		code, e := setup.IssueCode(db)
		if e != nil {
			return e
		}
		fmt.Println("一次性设置码（15 分钟有效，请勿分享）：\n" + code)
		return nil
	case "token":
		op := "show"
		if fs.NArg() > 0 {
			op = fs.Arg(0)
		}
		if op == "reset" {
			token, e := cfg.RotateToken()
			if e != nil {
				return e
			}
			fmt.Println("新访问令牌（旧会话已撤销）：\n" + token)
			return nil
		}
		if op != "show" {
			return errors.New("token 操作应为 show 或 reset")
		}
		b, e := db.Get("access_token")
		if e != nil {
			return errors.New("暂无访问令牌，请先完成初始化")
		}
		fmt.Println(b)
		return nil
	case "serve":
	default:
		return fmt.Errorf("未知命令 %q；支持 serve、setup-code、token、version", cmd)
	}
	if cmd == "serve" {
		release, err := lockDataDir(db.Dir)
		if err != nil {
			return err
		}
		defer release()
	}
	mem, e := memory.New(db.DB)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runner := tools.New(cfg.Get, []string{db.Dir})
	engine := agent.New(ctx, db, cfg, mem, runner)
	router := channels.New(ctx, db, cfg, engine)
	engine.Done = router.TaskDone
	engine.Authorize = router.AuthorizeTask
	router.Start()
	defer router.Close()
	watcher := watch.New(cfg.Get, func(ctx context.Context) (tools.Metrics, error) { return tools.Sample(ctx, cfg.Get().WorkDir) }, func(a watch.Alert) {
		at := a.At.UTC().Format(time.RFC3339Nano)
		_, _ = db.DB.Exec("INSERT OR IGNORE INTO notifications VALUES(?,?,?,?,?,?,?)", a.ID, a.Metric, a.State, a.Message, a.Value, a.Threshold, at)
		_, _ = db.Emit("watch", "", "", a)
		c := cfg.Get().Watch
		if c.NotifyChannel != "" && c.NotifyUser != "" {
			notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if e := router.Notify(notifyCtx, c.NotifyChannel, c.NotifyUser, cfg.Redact(a.Message)); e != nil {
				_, _ = db.Emit("channel", "", "", map[string]string{"message": "巡检推送未发送，详情保留在网页通知", "reason": cfg.Redact(e.Error())})
			}
		}
	})
	// The watcher persists its own state through the periodic snapshot below.
	if raw, e := db.Get("watch_state"); e == nil {
		var state watch.State
		if json.Unmarshal([]byte(raw), &state) == nil {
			watcher.Restore(state)
		}
	}
	go watcher.Run(ctx)
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				b, _ := json.Marshal(watcher.Snapshot())
				_ = db.Set("watch_state", string(b))
				_ = db.Cleanup(ctx, cfg.Get().Limits.RetentionDays)
			}
		}
	}()
	ln, e := net.Listen("tcp", *listen)
	if e != nil {
		return e
	}
	defer ln.Close()
	origin := *public
	if origin == "" {
		host, _, err := net.SplitHostPort(ln.Addr().String())
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return errors.New("非本机监听必须显式配置 --public-url https://域名 并使用 HTTPS 反向代理")
		}
		origin = "http://" + ln.Addr().String()
	}
	handler, e := server.New(db, cfg, mem, engine, router, server.Options{Origin: origin})
	if e != nil {
		return e
	}
	httpServer := &http.Server{Handler: handler.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		stopCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
		defer stop()
		_ = httpServer.Shutdown(stopCtx)
	}()
	log.Printf("NEXUS / OPS %s listening at %s", server.Version, origin)
	if !db.Initialized() {
		log.Print("尚未初始化：在本机运行 ops-agent setup-code --data-dir <同一数据目录> 获取一次性设置码")
	}
	e = httpServer.Serve(ln)
	cancel()
	router.Close()
	engine.Wait()
	b, _ := json.Marshal(watcher.Snapshot())
	_ = db.Set("watch_state", string(b))
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
