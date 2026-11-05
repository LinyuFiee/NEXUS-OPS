// Package channels owns channel lifecycle, pairing, authorization and delivery.
// Protocol clients are deliberately unaware of owner permissions and dry-run.
package channels

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"opsagent/internal/agent"
	"opsagent/internal/channels/onebot"
	"opsagent/internal/channels/qqofficial"
	"opsagent/internal/config"
	"opsagent/internal/store"
)

const (
	queueLimit   = 64
	mergeLimit   = 8
	inputLimit   = 12000
	replyLimit   = 300
	mergeWindow  = 700 * time.Millisecond
	pairLifetime = 10 * time.Minute
	pairLimit    = 32
)

var (
	errUnavailable       = errors.New("渠道未启用、尚未初始化或配置已变更")
	errUnauthorized      = errors.New("渠道任务的授权已失效")
	errStorage           = errors.New("无法更新渠道记录")
	ErrDryRun            = errors.New("渠道处于 dry-run，仅在网页记录预览")
	ErrActiveUnsupported = errors.New("QQ 官方渠道不支持主动通知，已保留到网页收件箱")
)

type officialClient interface {
	Verify([]byte, string, string, time.Time) error
	Challenge([]byte) ([]byte, bool, error)
	Parse([]byte) ([]qqofficial.Event, error)
	Send(context.Context, qqofficial.Event, string) error
	Check(context.Context) error
	Status() string
}

type oneBotClient interface {
	Run(context.Context, func(onebot.Event)) error
	Send(context.Context, onebot.Event, string) error
	Check(context.Context) error
	Status() string
}

type taskEngine interface {
	Submit(string, string, bool) (store.Task, error)
	Cancel(string) error
}

type channelEvent struct {
	ID, BotID, UserID, GroupID, Text, MessageID string
	IsGroup, Mentioned                          bool
}

type instance struct {
	id, channel, botID string
	fingerprint        [32]byte
	ctx                context.Context
	cancel             context.CancelFunc
	ready              bool
	state              string
	official           officialClient
	onebot             oneBotClient
	runDone            chan struct{}
}

func (i *instance) send(ctx context.Context, e channelEvent, text string) error {
	if i.official != nil {
		return i.official.Send(ctx, qqofficial.Event{ID: e.ID, BotID: e.BotID, UserID: e.UserID, GroupID: e.GroupID, Text: e.Text, MessageID: e.MessageID, IsGroup: e.IsGroup, Mentioned: e.Mentioned}, text)
	}
	if i.onebot != nil {
		return i.onebot.Send(ctx, onebot.Event{ID: e.ID, BotID: e.BotID, UserID: e.UserID, GroupID: e.GroupID, Text: e.Text, MessageID: e.MessageID, IsGroup: e.IsGroup, Mentioned: e.Mentioned}, text)
	}
	return errUnavailable
}

// The binding ID prevents revoke/re-pair from reviving old conversations. The
// instance ID invalidates queued work on restart or any channel configuration
// change. Keys contain no credentials and are never sent to a channel.
type routeAuthority struct {
	Channel   string `json:"channel"`
	Instance  string `json:"instance"`
	BotID     string `json:"bot_id"`
	UserID    string `json:"user_id"`
	GroupID   string `json:"group_id,omitempty"`
	IsGroup   bool   `json:"is_group,omitempty"`
	BindingID string `json:"binding_id"`
}

func (a routeAuthority) key() string { b, _ := json.Marshal(a); return "route:v1:" + string(b) }
func (a routeAuthority) scope() string {
	if a.IsGroup {
		return "group:" + a.Channel + ":" + a.BotID + ":" + a.GroupID
	}
	return "owner"
}
func (a routeAuthority) sessionChannel() string { return a.Channel + ":" + a.BotID }

type incoming struct {
	instance *instance
	event    channelEvent
}
type batch struct {
	origin       taskOrigin
	text         string
	runes, count int
	due          time.Time
}
type taskOrigin struct {
	authority routeAuthority
	event     channelEvent
	dry       bool
}
type outgoing struct {
	origin taskOrigin
	task   store.Task
	text   string
}

// Router starts no I/O until Start/Reload. All receiving, merging and final
// replies use fixed workers, not one goroutine or timer per remote sender.
type Router struct {
	ctx    context.Context
	cancel context.CancelFunc
	db     *store.Store
	cfg    *config.Manager
	engine taskEngine

	lifeMu          sync.Mutex
	mu              sync.RWMutex
	started, closed bool
	instances       map[string]*instance
	wg              sync.WaitGroup

	// Submit holds tasksMu until its returned ID is registered. Done can run
	// immediately on another goroutine, but cannot lose the reply correlation.
	// AuthorizeTask uses persisted session authority instead of this mutex.
	tasksMu       sync.Mutex
	tasks         map[string]taskOrigin
	receive       chan incoming
	receiveSlots  chan struct{}
	replies       chan outgoing
	callbackSlots chan struct{}
	checkSlots    chan struct{}
	window        time.Duration

	newOfficial func(qqofficial.Options) (officialClient, error)
	newOneBot   func(onebot.Options) (oneBotClient, error)
}

func New(ctx context.Context, db *store.Store, cfg *config.Manager, engine *agent.Engine) *Router {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &Router{ctx: ctx, cancel: cancel, db: db, cfg: cfg,
		instances: make(map[string]*instance), tasks: make(map[string]taskOrigin),
		receive: make(chan incoming, queueLimit), receiveSlots: make(chan struct{}, queueLimit),
		replies: make(chan outgoing, queueLimit), callbackSlots: make(chan struct{}, 8), checkSlots: make(chan struct{}, 2), window: mergeWindow,
		newOfficial: func(o qqofficial.Options) (officialClient, error) { return qqofficial.New(o) },
		newOneBot:   func(o onebot.Options) (oneBotClient, error) { return onebot.New(o) },
	}
	if engine != nil {
		r.engine = engine
	}
	return r
}

func channelConfig(c config.Config, name string) (config.Channel, bool) {
	switch name {
	case "official":
		return c.Channels.Official, true
	case "onebot":
		return c.Channels.OneBot, true
	}
	return config.Channel{}, false
}
func botID(name string, c config.Channel) string {
	if name == "official" {
		return c.AppID
	}
	return c.BotID
}
func fingerprint(name string, c config.Channel, s config.Secrets) [32]byte {
	secret := s.OneBotToken
	if name == "official" {
		secret = s.OfficialSecret
	}
	b, _ := json.Marshal(struct {
		Channel config.Channel
		Secret  string
	}{c, secret})
	return sha256.Sum256(b)
}

func (r *Router) Start() { r.Reload() }

// Reload is idempotent for unchanged channel settings. A changed instance is
// cancelled before its replacement can connect; no accepted work is replayed.
func (r *Router) Reload() {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	c, secrets := r.cfg.Get(), r.cfg.Secrets()
	initialized := r.db.Initialized()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if !r.started {
		r.started = true
		r.wg.Add(2)
		go r.receiveLoop()
		go r.replyLoop()
	}
	var old []*instance
	var fresh []*instance
	for _, name := range []string{"official", "onebot"} {
		ch, _ := channelConfig(c, name)
		sum := fingerprint(name, ch, secrets)
		previous := r.instances[name]
		ended := false
		if previous != nil && previous.runDone != nil {
			select {
			case <-previous.runDone:
				ended = true
			default:
			}
		}
		if previous != nil && previous.fingerprint == sum && !ended && (previous.state != "not_initialized") == initialized {
			continue
		}
		if previous != nil {
			previous.cancel()
			old = append(old, previous)
		}
		ctx, cancel := context.WithCancel(r.ctx)
		i := &instance{id: store.ID(), channel: name, botID: botID(name, ch), fingerprint: sum, ctx: ctx, cancel: cancel, state: "disabled"}
		if !initialized {
			i.state = "not_initialized"
		} else if ch.Enabled {
			i.state = "configuration_error"
			var err error
			if name == "official" {
				i.official, err = r.newOfficial(qqofficial.Options{AppID: ch.AppID, Secret: secrets.OfficialSecret})
			} else {
				i.onebot, err = r.newOneBot(onebot.Options{URL: ch.URL, Token: secrets.OneBotToken, BotID: ch.BotID})
			}
			if err == nil {
				i.ready = true
				i.state = "ready"
			}
		}
		r.instances[name] = i
		fresh = append(fresh, i)
	}
	r.mu.Unlock()
	for _, i := range old {
		if i.runDone != nil {
			<-i.runDone
		}
		r.cancelTasks(func(o taskOrigin) bool { return o.authority.Instance == i.id })
	}
	for _, i := range fresh {
		if !i.ready || i.onebot == nil {
			continue
		}
		i.runDone = make(chan struct{})
		r.wg.Add(1)
		go func(i *instance) {
			defer r.wg.Done()
			defer close(i.runDone)
			err := i.onebot.Run(i.ctx, func(e onebot.Event) {
				r.enqueue(i, channelEvent{ID: e.ID, BotID: e.BotID, UserID: e.UserID, GroupID: e.GroupID, Text: e.Text, MessageID: e.MessageID, IsGroup: e.IsGroup, Mentioned: e.Mentioned})
			})
			if err != nil && i.ctx.Err() == nil {
				r.emit("", "", i.channel, "connection_failed", "渠道连接已停止；请检查设置后显式重试", "")
			}
		}(i)
	}
}

func (r *Router) Close() {
	r.lifeMu.Lock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.lifeMu.Unlock()
		return
	}
	r.closed = true
	r.cancel()
	for _, i := range r.instances {
		i.cancel()
	}
	r.mu.Unlock()
	r.lifeMu.Unlock()
	r.cancelTasks(func(taskOrigin) bool { return true })
}
func (r *Router) Wait() { r.wg.Wait() }

func (r *Router) cancelTasks(match func(taskOrigin) bool) {
	r.tasksMu.Lock()
	var ids []string
	for id, o := range r.tasks {
		if match(o) {
			ids = append(ids, id)
			delete(r.tasks, id)
		}
	}
	r.tasksMu.Unlock()
	if r.engine != nil {
		for _, id := range ids {
			_ = r.engine.Cancel(id)
		}
	}
}

// current also compares the current manager snapshot, closing the gap between
// config.Save and Reload. Manager methods are never called while holding a
// manager lock, and no router lock is held across engine.Cancel.
func (r *Router) current(name string) (*instance, config.Channel, error) {
	c, s := r.cfg.Get(), r.cfg.Secrets()
	ch, ok := channelConfig(c, name)
	if !ok || !ch.Enabled {
		return nil, ch, errUnavailable
	}
	r.mu.RLock()
	i := r.instances[name]
	live := r.started && !r.closed && i != nil && i.ready
	r.mu.RUnlock()
	if !live || i.ctx.Err() != nil || i.fingerprint != fingerprint(name, ch, s) {
		return nil, ch, errUnavailable
	}
	return i, ch, nil
}

type channelStatus struct {
	Status  string `json:"status"`
	Enabled bool   `json:"enabled"`
	DryRun  bool   `json:"dry_run"`
}

func (r *Router) Status() any {
	c := r.cfg.Get()
	out := make(map[string]channelStatus, 2)
	for _, name := range []string{"official", "onebot"} {
		ch, _ := channelConfig(c, name)
		status := "disabled"
		if ch.Enabled {
			r.mu.RLock()
			i, closed, started := r.instances[name], r.closed, r.started
			r.mu.RUnlock()
			status = "stopped"
			if !closed && started && i != nil {
				status = i.state
				if current, _, err := r.current(name); err == nil && current == i {
					if i.official != nil {
						status = i.official.Status()
						if status == "idle" {
							status = "waiting_callback"
						}
					}
					if i.onebot != nil {
						status = i.onebot.Status()
					}
				} else if i.ready {
					status = "reload_required"
				}
			}
		}
		out[name] = channelStatus{Status: r.cfg.Redact(status), Enabled: ch.Enabled, DryRun: ch.DryRun}
	}
	return out
}

// Check is explicit, bounded and never calls Send or starts an event listener.
// It may test a configured-but-disabled channel without enabling that channel.
func (r *Router) Check(ctx context.Context, channel string) error {
	if !r.db.Initialized() || r.ctx.Err() != nil {
		return errUnavailable
	}
	c, s := r.cfg.Get(), r.cfg.Secrets()
	ch, ok := channelConfig(c, channel)
	if !ok {
		return errors.New("未知渠道")
	}
	select {
	case r.checkSlots <- struct{}{}:
		defer func() { <-r.checkSlots }()
	default:
		return errors.New("已有连接测试正在进行")
	}
	ctx, cancel := r.operationContext(ctx, r.ctx)
	defer cancel()
	var err error
	if channel == "official" {
		var client officialClient
		client, err = r.newOfficial(qqofficial.Options{AppID: ch.AppID, Secret: s.OfficialSecret})
		if err == nil {
			err = client.Check(ctx)
		}
	} else {
		var client oneBotClient
		if i, _, e := r.current(channel); e == nil {
			client = i.onebot
		} else {
			client, err = r.newOneBot(onebot.Options{URL: ch.URL, Token: s.OneBotToken, BotID: ch.BotID})
		}
		if err == nil {
			err = client.Check(ctx)
		}
	}
	if err != nil {
		r.emit("", "", channel, "check_failed", "连接测试失败；未发送业务消息", "")
		return errors.New("渠道连接测试失败，请检查凭据、地址及机器人身份")
	}
	r.emit("", "", channel, "check_ok", "连接测试通过；未发送业务消息，且不代表官方回调可达", "")
	return nil
}

// Callback is a public signed protocol endpoint, not a browser API. Origin and
// cookies are intentionally irrelevant. It does no database/model/network work
// before ACK and reads at most 1 MiB under a bounded verification semaphore.
func (r *Router) Callback(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		callbackError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	i, _, err := r.current("official")
	if err != nil || i.official == nil {
		callbackError(w, http.StatusForbidden, "官方渠道不可用")
		return
	}
	select {
	case r.callbackSlots <- struct{}{}:
		defer func() { <-r.callbackSlots }()
	default:
		callbackError(w, http.StatusServiceUnavailable, "回调繁忙")
		return
	}
	if req.ContentLength > qqofficial.MaxBodyBytes {
		callbackError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, qqofficial.MaxBodyBytes)
	defer req.Body.Close()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			callbackError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		} else {
			callbackError(w, http.StatusBadRequest, "无法读取回调")
		}
		return
	}
	if i.official.Verify(body, req.Header.Get(qqofficial.HeaderSignature), req.Header.Get(qqofficial.HeaderTimestamp), time.Now()) != nil {
		callbackError(w, http.StatusUnauthorized, "回调签名无效")
		return
	}
	response, handled, err := i.official.Challenge(body)
	if err != nil {
		callbackError(w, http.StatusBadRequest, "回调数据无效")
		return
	}
	if current, _, e := r.current("official"); e != nil || current != i {
		callbackError(w, http.StatusForbidden, "官方渠道不可用")
		return
	}
	if handled {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
		return
	}
	events, err := i.official.Parse(body)
	if err != nil || len(events) > mergeLimit {
		callbackError(w, http.StatusBadRequest, "回调数据无效")
		return
	}
	for _, e := range events {
		if !r.enqueue(i, channelEvent{ID: e.ID, BotID: e.BotID, UserID: e.UserID, GroupID: e.GroupID, Text: e.Text, MessageID: e.MessageID, IsGroup: e.IsGroup, Mentioned: e.Mentioned}) {
			callbackError(w, http.StatusServiceUnavailable, "接收队列已满或渠道已停止")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"op":12,"d":0}`)
}
func callbackError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (r *Router) enqueue(i *instance, e channelEvent) bool {
	if i == nil || i.ctx.Err() != nil || r.ctx.Err() != nil {
		return false
	}
	// One shared budget covers both unread events and buffered merge batches.
	select {
	case r.receiveSlots <- struct{}{}:
	default:
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || !r.started || r.instances[i.channel] != i {
		<-r.receiveSlots
		return false
	}
	select {
	case r.receive <- incoming{i, e}:
		return true
	default:
		<-r.receiveSlots
		return false
	}
}
func (r *Router) release(n int) {
	for j := 0; j < n; j++ {
		<-r.receiveSlots
	}
}

func (r *Router) receiveLoop() {
	defer r.wg.Done()
	batches := make(map[string]*batch)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	defer func() {
		r.mu.Lock()
		r.started = false // Fence producers before draining on parent cancellation.
		r.mu.Unlock()
		for _, b := range batches {
			r.release(b.count)
		}
		for {
			select {
			case <-r.receive:
				r.release(1)
			default:
				return
			}
		}
	}()
	lastPrune := time.Now()
	for {
		select {
		case <-r.ctx.Done():
			return
		case in := <-r.receive:
			r.accept(in, batches)
		case now := <-tick.C:
			for key, b := range batches {
				if !now.Before(b.due) {
					delete(batches, key)
					r.submit(b)
					r.release(b.count)
				}
			}
			if now.Sub(lastPrune) >= time.Second {
				r.pruneTasks()
				lastPrune = now
			}
		}
	}
}

func validIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func validMessageID(s string) bool {
	if len(s) == 0 || len(s) > 1024 || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) < 0
}
func allowedGroup(ch config.Channel, group string) bool {
	for _, id := range ch.Groups {
		if id == group {
			return true
		}
	}
	return false
}
func command(text, name string) (string, bool) {
	if text == name {
		return "", true
	}
	if strings.HasPrefix(text, name) {
		rest := strings.TrimPrefix(text, name)
		c, _ := utf8.DecodeRuneInString(rest)
		if unicode.IsSpace(c) {
			return strings.TrimSpace(rest), true
		}
	}
	return text, false
}
func digest(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (r *Router) accept(in incoming, batches map[string]*batch) {
	retained := false
	defer func() {
		if !retained {
			r.release(1)
		}
	}()
	i, ch, err := r.current(in.instance.channel)
	e := in.event
	if err != nil || i != in.instance || !validMessageID(e.ID) || !validMessageID(e.MessageID) || !validIdentifier(e.BotID) || !validIdentifier(e.UserID) || e.BotID != i.botID || e.UserID == e.BotID || !utf8.ValidString(e.Text) {
		return
	}
	e.Text = strings.TrimSpace(e.Text)
	if utf8.RuneCountInString(e.Text) > inputLimit {
		return
	}
	if e.IsGroup {
		if !validIdentifier(e.GroupID) || !allowedGroup(ch, e.GroupID) {
			return
		}
		text, prefixed := command(e.Text, "/ops")
		if !e.Mentioned && !prefixed {
			return
		}
		e.Text = text
	} else if e.GroupID != "" {
		return
	}
	if e.Text == "" {
		return
	}
	if code, binding := command(e.Text, "/bind"); binding {
		r.claimPair(i, e, code)
		return
	}
	bindingID, err := r.binding(i.channel, e.BotID, e.UserID)
	if err != nil {
		return
	}
	target := "user:" + e.UserID
	if e.IsGroup {
		target = "group:" + e.GroupID
	}
	seen, err := r.db.Seen("channel:" + digest(i.channel, e.BotID, target, e.MessageID))
	if err != nil || seen {
		return
	}
	a := routeAuthority{Channel: i.channel, Instance: i.id, BotID: e.BotID, UserID: e.UserID, GroupID: e.GroupID, IsGroup: e.IsGroup, BindingID: bindingID}
	key := a.key()
	n := utf8.RuneCountInString(e.Text)
	b := batches[key]
	if b != nil && b.runes+1+n > inputLimit {
		delete(batches, key)
		r.submit(b)
		r.release(b.count)
		b = nil
	}
	if b == nil {
		b = &batch{origin: taskOrigin{authority: a, event: e, dry: ch.DryRun}, text: e.Text, runes: n, count: 1, due: time.Now().Add(r.window)}
		batches[key] = b
	} else {
		b.text += "\n" + e.Text
		b.runes += 1 + n
		b.count++
		b.origin.event = e // Reply to the newest passive-message ID, once only.
	}
	retained = true
	if b.count >= mergeLimit {
		delete(batches, key)
		r.submit(b)
		r.release(b.count)
	}
}

func (r *Router) binding(channel, bot, user string) (string, error) {
	var id string
	err := r.db.DB.QueryRow("SELECT id FROM bindings WHERE channel=? AND bot_id=? AND user_id=?", channel, bot, user).Scan(&id)
	return id, err
}

func (r *Router) authorize(a routeAuthority) (*instance, config.Channel, error) {
	i, ch, err := r.current(a.Channel)
	if err != nil || i.id != a.Instance || i.botID != a.BotID || !validIdentifier(a.UserID) || a.UserID == a.BotID || a.BindingID == "" {
		return nil, ch, errUnauthorized
	}
	if a.IsGroup {
		if !validIdentifier(a.GroupID) || !allowedGroup(ch, a.GroupID) {
			return nil, ch, errUnauthorized
		}
	} else if a.GroupID != "" {
		return nil, ch, errUnauthorized
	}
	var id string
	err = r.db.DB.QueryRow("SELECT id FROM bindings WHERE id=? AND channel=? AND bot_id=? AND user_id=?", a.BindingID, a.Channel, a.BotID, a.UserID).Scan(&id)
	if err != nil {
		return nil, ch, errUnauthorized
	}
	return i, ch, nil
}

// AuthorizeTask must be installed on Engine.Authorize, which checks before
// execution, every model turn and after approvals. Router-side checks alone
// cannot revoke a tool that the engine has already begun executing.
func (r *Router) AuthorizeTask(t store.Task) error {
	s, err := r.db.Session(t.SessionID)
	if err != nil {
		return errUnauthorized
	}
	if s.Channel == "web" && s.Scope == "owner" {
		return nil
	}
	var key string
	if r.db.DB.QueryRow("SELECT key FROM channel_sessions WHERE session_id=?", s.ID).Scan(&key) != nil || !strings.HasPrefix(key, "route:v1:") {
		return errUnauthorized
	}
	var a routeAuthority
	if json.Unmarshal([]byte(strings.TrimPrefix(key, "route:v1:")), &a) != nil || a.scope() != s.Scope || a.sessionChannel() != s.Channel {
		return errUnauthorized
	}
	_, ch, err := r.authorize(a)
	if err != nil || ch.DryRun && !t.DryRun {
		return errUnauthorized
	}
	return nil
}

func (r *Router) session(a routeAuthority, title string) (string, error) {
	// Manager.Save holds the configuration lock while using SQLite. Never
	// acquire a manager lock after starting a transaction on its only connection.
	title = r.cfg.Redact(title)
	tx, err := r.db.DB.Begin()
	if err != nil {
		return "", errStorage
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow("SELECT session_id FROM channel_sessions WHERE key=?", a.key()).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", errStorage
	}
	id = store.ID()
	if chars := []rune(title); len(chars) > 70 {
		title = string(chars[:70])
	}
	now := store.Now()
	if _, err = tx.Exec("INSERT INTO sessions(id,title,scope,channel,created_at,updated_at) VALUES(?,?,?,?,?,?)", id, title, a.scope(), a.sessionChannel(), now, now); err != nil {
		return "", errStorage
	}
	if _, err = tx.Exec("INSERT INTO channel_sessions(key,session_id) VALUES(?,?)", a.key(), id); err != nil {
		return "", errStorage
	}
	if tx.Commit() != nil {
		return "", errStorage
	}
	return id, nil
}

func (r *Router) submit(b *batch) {
	if r.engine == nil || r.ctx.Err() != nil {
		return
	}
	_, ch, err := r.authorize(b.origin.authority)
	if err != nil {
		return
	}
	sessionID, err := r.session(b.origin.authority, b.text)
	if err != nil {
		r.emit("", "", b.origin.authority.Channel, "rejected", "无法创建渠道会话", "")
		return
	}
	r.tasksMu.Lock()
	if len(r.tasks) >= queueLimit || r.ctx.Err() != nil {
		r.tasksMu.Unlock()
		r.emit("", sessionID, b.origin.authority.Channel, "rejected", "渠道任务队列已满或已停止", "")
		return
	}
	dry := b.origin.dry || ch.DryRun
	t, err := r.engine.Submit(sessionID, b.text, dry)
	if err == nil {
		o := b.origin
		o.event.Text = ""
		o.dry = dry
		r.tasks[t.ID] = o
	}
	r.tasksMu.Unlock()
	if err != nil {
		r.emit("", sessionID, b.origin.authority.Channel, "rejected", "任务未受理；请在网页检查模型配置、输入或队列", "")
	}
}

func (r *Router) pruneTasks() {
	r.tasksMu.Lock()
	ids := make([]string, 0, len(r.tasks))
	for id := range r.tasks {
		ids = append(ids, id)
	}
	r.tasksMu.Unlock()
	for _, id := range ids {
		t, err := r.db.Task(id)
		// Queued cancellations do not invoke Engine.Done. Completed tasks do,
		// so leave them for that callback rather than racing away their reply.
		if errors.Is(err, sql.ErrNoRows) || err == nil && (t.Status == "cancelled" || t.Status == "interrupted") {
			r.tasksMu.Lock()
			delete(r.tasks, id)
			r.tasksMu.Unlock()
		}
	}
}

// TaskDone consumes correlation before queuing a final reply. Repeated Done
// callbacks, failures and reconnects never retry or produce progress messages.
func (r *Router) TaskDone(t store.Task) {
	r.tasksMu.Lock()
	o, ok := r.tasks[t.ID]
	if ok {
		delete(r.tasks, t.ID)
	}
	r.tasksMu.Unlock()
	if !ok || r.ctx.Err() != nil {
		return
	}
	text := t.Output
	if o.authority.IsGroup {
		text = "任务已完成；群聊未执行本地工具，详细信息请在网页查看。"
		if t.Status != "completed" {
			text = "任务未完成；群聊未执行本地工具，请在网页查看详情。"
		}
	} else if t.Status != "completed" {
		text = "任务未完成，请在网页查看详情。"
	} else if strings.TrimSpace(text) == "" {
		text = "任务已完成，详情请在网页查看。"
	}
	// Keep only bounded delivery metadata, not an entire model result/input in
	// each queued reply. The complete task stays in the web timeline/database.
	job := outgoing{origin: o, task: store.Task{ID: t.ID, SessionID: t.SessionID, Status: t.Status, DryRun: t.DryRun}, text: r.replyText(text)}
	r.mu.RLock()
	if r.closed || !r.started || r.ctx.Err() != nil {
		r.mu.RUnlock()
		return
	}
	queued := false
	select {
	case r.replies <- job:
		queued = true
	default:
	}
	r.mu.RUnlock()
	if !queued {
		r.emit(t.ID, t.SessionID, o.authority.Channel, "reply_dropped", "回复队列已满；仅保留网页结果，不重试", "")
	}
}

func (r *Router) replyLoop() {
	defer r.wg.Done()
	defer func() {
		// Fence a Done callback racing parent-context cancellation, then drop all
		// in-memory replies. None may be resumed after a service restart.
		r.mu.Lock()
		r.started = false
		r.mu.Unlock()
		for {
			select {
			case <-r.replies:
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-r.ctx.Done():
			return
		case job := <-r.replies:
			r.deliver(job)
		}
	}
}
func (r *Router) deliver(job outgoing) {
	o, t := job.origin, job.task
	text := r.replyText(job.text)
	i, ch, err := r.authorize(o.authority)
	if err != nil {
		r.emit(t.ID, t.SessionID, o.authority.Channel, "reply_suppressed", "授权或渠道已变更，未发送回复", "")
		return
	}
	if o.dry || t.DryRun || ch.DryRun {
		r.emit(t.ID, t.SessionID, o.authority.Channel, "dry_run", "仅预览，未向渠道发送", text)
		return
	}
	ctx, cancel := r.operationContext(r.ctx, i.ctx)
	defer cancel()
	// This is a single delivery attempt. Protocol clients never replay a
	// business message after an ambiguous disconnect.
	if err = i.send(ctx, o.event, text); err != nil {
		r.emit(t.ID, t.SessionID, o.authority.Channel, "reply_failed", "回复失败或投递结果未知；不自动重试，请查看网页结果", "")
		return
	}
	r.emit(t.ID, t.SessionID, o.authority.Channel, "replied", "已发送一次最终回复", "")
}
func (r *Router) replyText(text string) string {
	text = r.cfg.Redact(strings.TrimSpace(text))
	chars := []rune(text)
	if len(chars) > replyLimit {
		suffix := "…（完整内容请在网页查看）"
		text = string(chars[:replyLimit-utf8.RuneCountInString(suffix)]) + suffix
	}
	return text
}
func (r *Router) operationContext(caller, owner context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(owner, 15*time.Second)
	if caller == nil {
		caller = context.Background()
	}
	stop := context.AfterFunc(caller, cancel)
	if caller.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}
func (r *Router) emit(taskID, sessionID, channel, status, message, text string) {
	data := map[string]string{"channel": channel, "status": status, "message": r.cfg.Redact(message)}
	if text != "" {
		data["text"] = r.replyText(text)
	}
	_, _ = r.db.Emit("channel", taskID, sessionID, data)
}

type Binding struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	BotID     string `json:"bot_id"`
	UserID    string `json:"user_id"`
	CreatedAt string `json:"created_at"`
}
type Pairing struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	Code      string `json:"code"`
	UserID    string `json:"user_id"`
	BotID     string `json:"bot_id"`
	ExpiresAt string `json:"expires_at"`
}

// Bindings/Pair/ApprovePair/RemoveBinding are only for authenticated web APIs.
// There is intentionally no bot command that approves, creates or revokes an
// owner binding. A remote /bind can only claim an existing web-generated code.
func (r *Router) Bindings() (map[string]any, error) {
	bindings := []Binding{}
	pending := []Pairing{}
	rows, err := r.db.DB.Query("SELECT id,channel,bot_id,user_id,created_at FROM bindings ORDER BY created_at")
	if err != nil {
		return nil, errStorage
	}
	for rows.Next() {
		var b Binding
		if rows.Scan(&b.ID, &b.Channel, &b.BotID, &b.UserID, &b.CreatedAt) != nil {
			rows.Close()
			return nil, errStorage
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, errStorage
	}
	rows, err = r.db.DB.Query("SELECT id,channel,code,user_id,bot_id,expires_at FROM pairings ORDER BY expires_at DESC LIMIT ?", pairLimit)
	if err != nil {
		return nil, errStorage
	}
	defer rows.Close()
	for rows.Next() {
		var p Pairing
		if rows.Scan(&p.ID, &p.Channel, &p.Code, &p.UserID, &p.BotID, &p.ExpiresAt) != nil {
			return nil, errStorage
		}
		expires, e := time.Parse(time.RFC3339Nano, p.ExpiresAt)
		if e == nil && time.Now().Before(expires) {
			pending = append(pending, p)
		}
	}
	if rows.Err() != nil {
		return nil, errStorage
	}
	return map[string]any{"bindings": bindings, "pending": pending}, nil
}

func (r *Router) Pair(channel string) (any, error) {
	i, _, err := r.current(channel)
	if err != nil {
		return nil, errUnavailable
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return nil, errors.New("无法生成安全配对码")
	}
	p := Pairing{ID: store.ID(), Channel: channel, Code: hex.EncodeToString(token[:]), BotID: i.botID, ExpiresAt: time.Now().Add(pairLifetime).UTC().Format(time.RFC3339Nano)}
	tx, err := r.db.DB.Begin()
	if err != nil {
		return nil, errStorage
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM pairings WHERE julianday(expires_at)<=julianday('now')"); err != nil {
		return nil, errStorage
	}
	var count int
	if tx.QueryRow("SELECT count(*) FROM pairings").Scan(&count) != nil {
		return nil, errStorage
	}
	if count >= pairLimit {
		return nil, errors.New("待配对请求过多，请处理或等待旧请求过期")
	}
	if _, err = tx.Exec("INSERT INTO pairings(id,channel,code,user_id,bot_id,expires_at) VALUES(?,?,?,?,?,?)", p.ID, p.Channel, p.Code, "", p.BotID, p.ExpiresAt); err != nil {
		return nil, errStorage
	}
	if tx.Commit() != nil {
		return nil, errStorage
	}
	return p, nil
}

func (r *Router) claimPair(i *instance, e channelEvent, code string) {
	if len(code) != 32 {
		return
	}
	if decoded, err := hex.DecodeString(code); err != nil || len(decoded) != 16 {
		return
	}
	// Single conditional UPDATE makes candidate acquisition one-shot, channel
	// and bot specific. It never creates a binding, even for an existing owner.
	res, err := r.db.DB.Exec("UPDATE pairings SET user_id=? WHERE channel=? AND bot_id=? AND code=? AND user_id='' AND julianday(expires_at)>julianday('now')", e.UserID, i.channel, e.BotID, code)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		r.emit("", "", i.channel, "pair_candidate", "已收到配对候选人；必须在已登录网页确认后才授权", "")
	}
}

func (r *Router) ApprovePair(id string) error {
	var p Pairing
	if r.db.DB.QueryRow("SELECT id,channel,code,user_id,bot_id,expires_at FROM pairings WHERE id=?", id).Scan(&p.ID, &p.Channel, &p.Code, &p.UserID, &p.BotID, &p.ExpiresAt) != nil {
		return errors.New("配对请求不存在或已处理")
	}
	expires, err := time.Parse(time.RFC3339Nano, p.ExpiresAt)
	if err != nil || !time.Now().Before(expires) || !validIdentifier(p.UserID) {
		return errors.New("配对请求已过期或尚无候选人")
	}
	i, _, err := r.current(p.Channel)
	if err != nil || i.botID != p.BotID || p.UserID == p.BotID {
		return errUnavailable
	}
	// Configuration locks are acquired before the SQLite transaction. Consuming
	// the exact candidate under the transaction keeps concurrent approvals and
	// expiry one-shot without reversing Manager.Save's lock order.
	tx, err := r.db.DB.Begin()
	if err != nil {
		return errStorage
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM pairings WHERE id=? AND channel=? AND bot_id=? AND user_id=? AND code=? AND julianday(expires_at)>julianday('now')", p.ID, p.Channel, p.BotID, p.UserID, p.Code)
	if err != nil {
		return errStorage
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errors.New("配对请求已过期或已处理")
	}
	if i.ctx.Err() != nil {
		return errUnavailable
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO bindings(id,channel,bot_id,user_id,created_at) VALUES(?,?,?,?,?)", store.ID(), p.Channel, p.BotID, p.UserID, store.Now()); err != nil {
		return errStorage
	}
	if tx.Commit() != nil {
		return errStorage
	}
	r.emit("", "", p.Channel, "bound", "已由网页确认渠道用户授权", "")
	return nil
}

func (r *Router) RemoveBinding(id string) error {
	res, err := r.db.DB.Exec("DELETE FROM bindings WHERE id=?", id)
	if err != nil {
		return errStorage
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("绑定不存在")
	}
	r.cancelTasks(func(o taskOrigin) bool { return o.authority.BindingID == id })
	r.emit("", "", "", "unbound", "绑定已撤销；缓冲、排队任务及回复均须重新验证授权", "")
	return nil
}

// Notify is only for explicit watch notifications to an authorized owner.
// QQ official can only reply to a real incoming message ID, so its active
// notifications go to the web inbox and return an explicit unsupported error.
func (r *Router) Notify(ctx context.Context, channel, user, text string) error {
	i, ch, err := r.current(channel)
	if err != nil {
		return errUnavailable
	}
	bindingID, err := r.binding(channel, i.botID, user)
	if err != nil || !validIdentifier(user) || user == i.botID {
		return errUnauthorized
	}
	a := routeAuthority{Channel: channel, Instance: i.id, BotID: i.botID, UserID: user, BindingID: bindingID}
	text = r.replyText(text)
	if text == "" {
		return errors.New("通知内容为空")
	}
	if ch.DryRun {
		r.emit("", "", channel, "dry_run", "值守通知仅预览，未向渠道发送", text)
		return ErrDryRun
	}
	if channel == "official" {
		_, err = r.db.DB.Exec("INSERT INTO notifications(id,metric,state,message,value,threshold,created_at) VALUES(?,?,?,?,?,?,?)", store.ID(), "channel", "web_only", "官方渠道不支持主动通知："+text, 0, 0, store.Now())
		if err != nil {
			return errStorage
		}
		r.emit("", "", channel, "web_only", "官方主动通知不受支持，已保留到网页收件箱", "")
		return ErrActiveUnsupported
	}
	// Recheck after preparing the notification and immediately before Send.
	i, ch, err = r.authorize(a)
	if err != nil {
		return errUnauthorized
	}
	if ch.DryRun {
		return ErrDryRun
	}
	ctx, cancel := r.operationContext(ctx, i.ctx)
	defer cancel()
	if i.send(ctx, channelEvent{BotID: i.botID, UserID: user}, text) != nil {
		r.emit("", "", channel, "notify_failed", "主动通知失败或结果未知，不自动重试", "")
		return errors.New("主动通知失败或投递结果未知")
	}
	r.emit("", "", channel, "notified", "已向授权用户发送一次通知", "")
	return nil
}
