package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"opsagent/internal/agent"
	"opsagent/internal/channels"
	"opsagent/internal/config"
	"opsagent/internal/memory"
	"opsagent/internal/persona"
	"opsagent/internal/provider"
	"opsagent/internal/setup"
	"opsagent/internal/store"
	"opsagent/internal/tools"
	"opsagent/web"
)

const Version = "0.1.0"

type Options struct {
	Origin               string
	AllowLoopbackDynamic bool
}
type limitEntry struct {
	Count int
	Until time.Time
}
type Server struct {
	db     *store.Store
	cfg    *config.Manager
	mem    *memory.Store
	engine *agent.Engine
	router *channels.Router
	opts   Options
	mu     sync.Mutex
	rates  map[string]limitEntry
}

func New(db *store.Store, cfg *config.Manager, mem *memory.Store, engine *agent.Engine, router *channels.Router, opts Options) (*Server, error) {
	u, e := url.Parse(opts.Origin)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("public URL 必须是一个完整 origin，不包含路径或凭据")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, errors.New("非本机访问必须配置 HTTPS public URL")
	}
	opts.Origin = strings.TrimRight(opts.Origin, "/")
	return &Server{db: db, cfg: cfg, mem: mem, engine: engine, router: router, opts: opts, rates: map[string]limitEntry{}}, nil
}
func isLoopback(s string) bool { return s == "localhost" || s == "127.0.0.1" || s == "::1" }
func (s *Server) originFor(r *http.Request) string {
	if s.opts.AllowLoopbackDynamic {
		host, _, e := net.SplitHostPort(r.Host)
		if e == nil && isLoopback(host) {
			return "http://" + r.Host
		}
	}
	return s.opts.Origin
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"initialized": s.db.Initialized(), "version": Version})
	})
	mux.HandleFunc("POST /api/setup/unlock", s.unlock)
	mux.HandleFunc("GET /api/setup/config", s.setupOnly(s.configuration))
	mux.HandleFunc("POST /api/setup/complete", s.setupOnly(s.complete))
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.auth(s.logout))
	mux.HandleFunc("GET /api/config", s.auth(s.configuration))
	mux.HandleFunc("PUT /api/config", s.auth(s.saveConfig))
	mux.HandleFunc("POST /api/token/rotate", s.auth(s.rotateToken))
	mux.HandleFunc("POST /api/test/model", s.eitherAuth(s.testModel))
	mux.HandleFunc("GET /api/metrics", s.auth(s.metrics))
	mux.HandleFunc("GET /api/sessions", s.auth(s.sessions))
	mux.HandleFunc("POST /api/sessions", s.auth(s.newSession))
	mux.HandleFunc("GET /api/sessions/{id}", s.auth(s.session))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.auth(s.deleteSession))
	mux.HandleFunc("GET /api/tasks", s.auth(s.tasks))
	mux.HandleFunc("POST /api/tasks", s.auth(s.submit))
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.auth(s.cancel))
	mux.HandleFunc("GET /api/approvals", s.auth(func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.engine.Approvals()) }))
	mux.HandleFunc("POST /api/approvals/{id}", s.auth(s.approve))
	mux.HandleFunc("GET /api/events", s.auth(s.events))
	mux.HandleFunc("GET /api/timeline", s.auth(s.timeline))
	mux.HandleFunc("GET /api/brief", s.auth(func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]string{"text": s.engine.Brief()})
	}))
	mux.HandleFunc("GET /api/memories", s.auth(s.memories))
	mux.HandleFunc("POST /api/memories", s.auth(s.saveMemory))
	mux.HandleFunc("GET /api/memories/search", s.auth(s.searchMemory))
	mux.HandleFunc("DELETE /api/memories/{id}", s.auth(s.deleteMemory))
	mux.HandleFunc("POST /api/memories/{id}/state", s.auth(s.memoryState))
	mux.HandleFunc("GET /api/channels", s.auth(s.channelStatus))
	mux.HandleFunc("POST /api/channels/{channel}/test", s.auth(s.channelTest))
	mux.HandleFunc("GET /api/bindings", s.auth(s.bindings))
	mux.HandleFunc("POST /api/bindings", s.auth(s.pair))
	mux.HandleFunc("POST /api/bindings/{id}/approve", s.auth(s.pairApprove))
	mux.HandleFunc("DELETE /api/bindings/{id}", s.auth(s.bindingDelete))
	mux.HandleFunc("GET /api/notifications", s.auth(s.notifications))
	mux.HandleFunc("POST /api/callback/qq", func(w http.ResponseWriter, r *http.Request) {
		if s.router == nil {
			fail(w, 503, "QQ 尚未启用")
			return
		}
		s.router.Callback(w, r)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "接口不存在") })
	assets := http.FileServer(http.FS(web.Files))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" {
			http.NotFound(w, r)
			return
		}
		assets.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("HTTP handler error (%T)", rec)
				fail(w, 500, "内部错误")
			}
		}()
		u, _ := url.Parse(s.originFor(r))
		if !strings.EqualFold(r.Host, u.Host) {
			fail(w, 403, "不受信任的访问地址")
			return
		}
		if r.URL.Path != "/api/callback/qq" {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != s.originFor(r) {
				fail(w, 403, "跨站请求被拒绝")
				return
			}
			if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
				if origin == "" && r.Header.Get("X-Ops-Request") != "1" {
					fail(w, 403, "请求缺少同源验证")
					return
				}
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					fail(w, 403, "跨站请求被拒绝")
					return
				}
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		bodyless := r.Method == "DELETE" || (r.Method == "POST" && (r.URL.Path == "/api/logout" || strings.HasSuffix(r.URL.Path, "/cancel") || (strings.HasPrefix(r.URL.Path, "/api/channels/") && strings.HasSuffix(r.URL.Path, "/test")) || (strings.HasPrefix(r.URL.Path, "/api/bindings/") && strings.HasSuffix(r.URL.Path, "/approve"))))
		if bodyless {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				fail(w, 400, "请求体无效或过大")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, text string) {
	respond(w, status, map[string]string{"error": text})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "请使用 application/json")
		return false
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "请求 JSON 无效或包含未知字段")
		return false
	}
	var more any
	if d.Decode(&more) != io.EOF {
		fail(w, 400, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func (s *Server) authenticated(r *http.Request, kind string) bool {
	if kind == "login" && !s.db.Initialized() {
		return false
	}
	if kind == "setup" && s.db.Initialized() {
		return false
	}
	c, e := r.Cookie("ops_" + kind)
	if e != nil || len(c.Value) > 128 {
		return false
	}
	var expires int64
	e = s.db.DB.QueryRow("SELECT expires FROM auth_sessions WHERE hash=? AND kind=?", config.Hash(c.Value), kind).Scan(&expires)
	return e == nil && expires > time.Now().Unix()
}
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r, "login") {
			fail(w, 401, "请先登录")
			return
		}
		next(w, r)
	}
}
func (s *Server) setupOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r, "setup") {
			fail(w, 401, "设置会话无效，请重新从本机获取设置码")
			return
		}
		next(w, r)
	}
}
func (s *Server) eitherAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r, "setup") && !s.authenticated(r, "login") {
			fail(w, 401, "请先验证身份")
			return
		}
		next(w, r)
	}
}
func (s *Server) cookie(w http.ResponseWriter, r *http.Request, kind, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: "ops_" + kind, Value: value, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.originFor(r), "https://"), SameSite: http.SameSiteStrictMode, MaxAge: age})
}
func (s *Server) limited(r *http.Request, bucket string) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	key := bucket + host
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.rates) > 1024 {
		for k, v := range s.rates {
			if now.After(v.Until) {
				delete(s.rates, k)
			}
		}
		if len(s.rates) > 1024 {
			return true
		}
	}
	v := s.rates[key]
	if now.After(v.Until) {
		v = limitEntry{Until: now.Add(time.Minute)}
	}
	v.Count++
	s.rates[key] = v
	return v.Count > 8
}
func (s *Server) unlock(w http.ResponseWriter, r *http.Request) {
	if s.limited(r, "setup") {
		fail(w, 429, "尝试过于频繁，请稍后再试")
		return
	}
	var p struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &p) {
		return
	}
	token, e := setup.Unlock(s.db, p.Code)
	if e != nil {
		fail(w, 403, e.Error())
		return
	}
	s.cookie(w, r, "setup", token, 1800)
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.limited(r, "login") {
		fail(w, 429, "登录尝试过于频繁")
		return
	}
	var p struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &p) {
		return
	}
	token, err := s.cfg.CreateSession(p.Token)
	if err != nil {
		fail(w, 403, "访问令牌无效或会话创建失败")
		return
	}
	s.cookie(w, r, "login", token, 86400)
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("ops_login"); e == nil {
		_, _ = s.db.DB.Exec("DELETE FROM auth_sessions WHERE hash=?", config.Hash(c.Value))
	}
	s.cookie(w, r, "login", "", -1)
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) environment() map[string]any {
	host, _ := os.Hostname()
	return map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "hostname": host, "data_dir": s.db.Dir, "work_dir": s.cfg.Get().WorkDir, "linux_tools": runtime.GOOS == "linux", "go_version": runtime.Version(), "note": "非 Linux 环境仅供界面与协议开发验证；系统命令需在 Linux 实机验证"}
}
func (s *Server) configuration(w http.ResponseWriter, r *http.Request) {
	sec := s.cfg.Secrets()
	respond(w, 200, map[string]any{"config": s.cfg.Get(), "presets": persona.Presets(), "environment": s.environment(), "secrets_configured": map[string]bool{"model_key": sec.ModelKey != "", "official_secret": sec.OfficialSecret != "", "onebot_token": sec.OneBotToken != ""}})
}

type configRequest struct {
	Config       config.Config  `json:"config"`
	Secrets      config.Secrets `json:"secrets"`
	ConfirmToken string         `json:"confirm_token"`
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	var p configRequest
	if !decode(w, r, &p) {
		return
	}
	token, e := s.cfg.Initialize(p.Config, p.Secrets)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	s.cookie(w, r, "setup", "", -1)
	respond(w, 200, map[string]string{"token": token})
}
func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	var p configRequest
	if !decode(w, r, &p) {
		return
	}
	if !s.cfg.VerifyToken(p.ConfirmToken) {
		fail(w, 403, "请重新输入有效的访问令牌")
		return
	}
	if e := s.cfg.Save(p.Config, p.Secrets); e != nil {
		fail(w, 400, e.Error())
		return
	}
	if s.router != nil {
		s.router.Reload()
	}
	_, _ = s.db.Emit("config", "", "", map[string]string{"message": "配置已更新"})
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	var p struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if !decode(w, r, &p) {
		return
	}
	if !s.cfg.VerifyToken(p.ConfirmToken) {
		fail(w, 403, "访问令牌无效")
		return
	}
	token, e := s.cfg.RotateToken()
	if e != nil {
		fail(w, 500, "令牌重置失败")
		return
	}
	s.cookie(w, r, "login", "", -1)
	respond(w, 200, map[string]string{"token": token})
}
func (s *Server) testModel(w http.ResponseWriter, r *http.Request) {
	if s.limited(r, "model-test") {
		fail(w, 429, "测试过于频繁")
		return
	}
	var p struct {
		Model  config.Model `json:"model"`
		APIKey string       `json:"api_key"`
	}
	if !decode(w, r, &p) {
		return
	}
	c := s.cfg.Get()
	c.Model = p.Model
	if e := c.Validate(); e != nil {
		fail(w, 400, e.Error())
		return
	}
	savedModel, savedKey := s.cfg.ModelCredentials()
	key := p.APIKey
	if key == "" {
		if strings.TrimRight(p.Model.BaseURL, "/") != strings.TrimRight(savedModel.BaseURL, "/") {
			fail(w, 400, "新地址测试请重新输入 API Key，保存的凭据不会发送到其他地址")
			return
		}
		key = savedKey
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	reply, e := provider.New(p.Model.BaseURL, p.Model.Name, key).Complete(ctx, []provider.Message{{Role: "user", Content: "Connection test. Reply OK only."}}, nil, nil)
	if e != nil {
		fail(w, 502, s.cfg.Redact(e.Error()))
		return
	}
	respond(w, 200, map[string]any{"ok": true, "reply": s.cfg.Redact(strings.ReplaceAll(reply.Content, key, "[已隐藏凭据]"))})
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	m, e := tools.Sample(ctx, s.cfg.Get().WorkDir)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, 200, m)
}
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	x, e := s.db.Sessions()
	if e != nil {
		fail(w, 500, "无法读取会话")
		return
	}
	respond(w, 200, x)
}
func (s *Server) newSession(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &p) {
		return
	}
	x, e := s.db.CreateSession(p.Title, "owner", "web")
	if e != nil {
		fail(w, 500, "无法创建会话")
		return
	}
	respond(w, 201, x)
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	x, e := s.db.Session(id)
	if e != nil {
		fail(w, 404, "会话不存在")
		return
	}
	messages, e := s.db.Messages(id, 100)
	if e != nil {
		fail(w, 500, "无法读取消息")
		return
	}
	tasks, _ := s.db.Tasks(id)
	respond(w, 200, map[string]any{"session": x, "messages": messages, "tasks": tasks})
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if e := s.db.DeleteSession(r.PathValue("id")); e != nil {
		fail(w, 409, e.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	x, e := s.db.Tasks("")
	if e != nil {
		fail(w, 500, "无法读取任务")
		return
	}
	respond(w, 200, x)
}
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var p struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
	}
	if !decode(w, r, &p) {
		return
	}
	if p.SessionID != "" {
		ses, e := s.db.Session(p.SessionID)
		if e != nil || ses.Channel != "web" {
			fail(w, 400, "网页任务只能发送到网页会话；QQ 会话仅供查看")
			return
		}
	}
	t, e := s.engine.Submit(p.SessionID, p.Message, false)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	respond(w, 202, t)
}
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if e := s.engine.Cancel(r.PathValue("id")); e != nil {
		fail(w, 409, e.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Approve *bool `json:"approve"`
	}
	if !decode(w, r, &p) {
		return
	}
	if p.Approve == nil {
		fail(w, 400, "缺少审批决定")
		return
	}
	if e := s.engine.Decide(r.PathValue("id"), *p.Approve); e != nil {
		fail(w, 409, e.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	x, e := s.db.Events(0, true)
	if e != nil {
		fail(w, 500, "无法读取时间线")
		return
	}
	respond(w, 200, x)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "不支持事件流")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		after, _ = strconv.ParseInt(last, 10, 64)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	keep := 0
	for {
		if !s.authenticated(r, "login") {
			return
		}
		events, e := s.db.Events(after, false)
		if e != nil {
			return
		}
		for _, v := range events {
			b, _ := json.Marshal(v)
			if _, e = fmt.Fprintf(w, "id: %d\nevent: update\ndata: %s\n\n", v.ID, b); e != nil {
				return
			}
			after = v.ID
		}
		keep++
		if keep%15 == 0 {
			_, _ = io.WriteString(w, ": keepalive\n\n")
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) memories(w http.ResponseWriter, r *http.Request) {
	x, e := s.mem.List(r.Context(), "", "", 200)
	if e != nil {
		fail(w, 500, "无法读取记忆")
		return
	}
	respond(w, 200, x)
}
func (s *Server) saveMemory(w http.ResponseWriter, r *http.Request) {
	var p memory.Item
	if !decode(w, r, &p) {
		return
	}
	if s.cfg.ContainsSecret(p.Text) {
		fail(w, 400, "记忆不得包含凭据")
		return
	}
	if p.Scope == "" {
		p.Scope = "owner"
	}
	if p.Category == "" {
		p.Category = "fact"
	}
	if p.Status == "" {
		p.Status = "active"
	}
	p.Source = "user"
	item, e := s.mem.Put(r.Context(), p)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	_ = s.db.ClearModelHistory()
	_, _ = s.db.Emit("memory", "", "", map[string]any{"action": "saved", "id": item.ID})
	respond(w, 200, item)
}
func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	s.engine.MemoryChanged()
	if e := s.mem.Delete(r.Context(), r.PathValue("id")); e != nil {
		fail(w, 400, e.Error())
		return
	}
	_ = s.db.ClearModelHistory()
	_, _ = s.db.DB.Exec("DELETE FROM events WHERE kind='memory'")
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) memoryState(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Status       string `json:"status"`
		SupersededBy string `json:"superseded_by"`
	}
	if !decode(w, r, &p) {
		return
	}
	if e := s.mem.SetStatus(r.Context(), r.PathValue("id"), p.Status, p.SupersededBy); e != nil {
		fail(w, 400, e.Error())
		return
	}
	_ = s.db.ClearModelHistory()
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) searchMemory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) > 2000 {
		fail(w, 400, "搜索内容过长")
		return
	}
	c := s.cfg.Get()
	x, e := s.mem.Search(r.Context(), q, []string{"owner"}, c.Memory.MaxResults, c.Memory.MaxChars)
	if e != nil {
		fail(w, 500, "检索失败")
		return
	}
	respond(w, 200, x)
}
func (s *Server) channelStatus(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		respond(w, 200, map[string]any{})
		return
	}
	respond(w, 200, s.router.Status())
}
func (s *Server) channelTest(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		fail(w, 503, "渠道未就绪")
		return
	}
	if s.limited(r, "channel-test") {
		fail(w, 429, "测试过于频繁")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if e := s.router.Check(ctx, r.PathValue("channel")); e != nil {
		fail(w, 502, s.cfg.Redact(e.Error()))
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) bindings(w http.ResponseWriter, r *http.Request) {
	x, e := s.router.Bindings()
	if e != nil {
		fail(w, 500, "无法读取绑定")
		return
	}
	respond(w, 200, x)
}
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Channel string `json:"channel"`
	}
	if !decode(w, r, &p) {
		return
	}
	x, e := s.router.Pair(p.Channel)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	respond(w, 201, x)
}
func (s *Server) pairApprove(w http.ResponseWriter, r *http.Request) {
	if e := s.router.ApprovePair(r.PathValue("id")); e != nil {
		fail(w, 400, e.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) bindingDelete(w http.ResponseWriter, r *http.Request) {
	if e := s.router.RemoveBinding(r.PathValue("id")); e != nil {
		fail(w, 400, e.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) notifications(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.DB.Query("SELECT id,metric,state,message,value,threshold,created_at FROM notifications ORDER BY created_at DESC LIMIT 100")
	if e != nil {
		fail(w, 500, "无法读取通知")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, metric, state, msg, at string
		var v, threshold float64
		if e = rows.Scan(&id, &metric, &state, &msg, &v, &threshold, &at); e != nil {
			break
		}
		out = append(out, map[string]any{"id": id, "metric": metric, "state": state, "message": msg, "value": v, "threshold": threshold, "created_at": at})
	}
	respond(w, 200, out)
}
