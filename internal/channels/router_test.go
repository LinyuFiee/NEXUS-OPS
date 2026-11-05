package channels

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"opsagent/internal/channels/onebot"
	"opsagent/internal/channels/qqofficial"
	"opsagent/internal/config"
	"opsagent/internal/store"
)

const testOfficialSecret = "unit-official-secret-value"

type sentMessage struct {
	event channelEvent
	text  string
}
type fakeOfficial struct {
	*qqofficial.Client
	mu                sync.Mutex
	sent              []sentMessage
	checks            int
	sendErr, checkErr error
}

func (f *fakeOfficial) Send(ctx context.Context, e qqofficial.Event, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{channelEvent{ID: e.ID, BotID: e.BotID, UserID: e.UserID, GroupID: e.GroupID, MessageID: e.MessageID, IsGroup: e.IsGroup}, text})
	return f.sendErr
}
func (f *fakeOfficial) Check(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return f.checkErr
}
func (f *fakeOfficial) messages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

type fakeOneBot struct {
	mu                sync.Mutex
	sent              []sentMessage
	checks, runs      int
	started, stopped  chan struct{}
	status            string
	sendErr, checkErr error
}

func (f *fakeOneBot) Run(ctx context.Context, handler func(onebot.Event)) error {
	f.mu.Lock()
	f.runs++
	f.status = "connected"
	f.mu.Unlock()
	close(f.started)
	<-ctx.Done()
	f.mu.Lock()
	f.status = "stopped"
	f.mu.Unlock()
	close(f.stopped)
	return ctx.Err()
}
func (f *fakeOneBot) Send(ctx context.Context, e onebot.Event, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{channelEvent{ID: e.ID, BotID: e.BotID, UserID: e.UserID, GroupID: e.GroupID, MessageID: e.MessageID, IsGroup: e.IsGroup}, text})
	return f.sendErr
}
func (f *fakeOneBot) Check(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return f.checkErr
}
func (f *fakeOneBot) Status() string { f.mu.Lock(); defer f.mu.Unlock(); return f.status }
func (f *fakeOneBot) messages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

type fakeEngine struct {
	db        *store.Store
	submitted chan store.Task
	cancelled chan string
	onSubmit  func(store.Task)
}

func (f *fakeEngine) Submit(session, input string, dry bool) (store.Task, error) {
	t, err := f.db.CreateTask(session, input, dry)
	if err == nil {
		if f.onSubmit != nil {
			f.onSubmit(t)
		}
		f.submitted <- t
	}
	return t, err
}
func (f *fakeEngine) Cancel(id string) error {
	select {
	case f.cancelled <- id:
	default:
	}
	return f.db.UpdateTask(id, "cancelled", "", "cancelled by test")
}

type harness struct {
	r        *Router
	db       *store.Store
	cfg      *config.Manager
	engine   *fakeEngine
	mu       sync.Mutex
	official []*fakeOfficial
	onebot   []*fakeOneBot
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(db, t.TempDir())
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err = cfg.Initialize(cfg.Get(), config.Secrets{ModelKey: "unit-model-secret-value", OfficialSecret: testOfficialSecret, OneBotToken: "unit-onebot-secret-value"}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	c := cfg.Get()
	c.Channels.Official = config.Channel{Enabled: true, DryRun: true, AppID: "bot-official", Groups: []string{"group-a", "group-b"}}
	c.Channels.OneBot = config.Channel{Enabled: true, DryRun: true, BotID: "10000", URL: "ws://127.0.0.1:65534", Groups: []string{"30000", "30001"}}
	if err = cfg.Save(c, config.Secrets{}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	h := &harness{db: db, cfg: cfg, engine: &fakeEngine{db: db, submitted: make(chan store.Task, 128), cancelled: make(chan string, 128)}}
	h.r = New(context.Background(), db, cfg, nil)
	h.r.engine = h.engine
	h.r.window = 50 * time.Millisecond
	h.r.newOfficial = func(o qqofficial.Options) (officialClient, error) {
		c, err := qqofficial.New(o)
		if err != nil {
			return nil, err
		}
		f := &fakeOfficial{Client: c}
		h.mu.Lock()
		h.official = append(h.official, f)
		h.mu.Unlock()
		return f, nil
	}
	h.r.newOneBot = func(o onebot.Options) (oneBotClient, error) {
		f := &fakeOneBot{started: make(chan struct{}), stopped: make(chan struct{}), status: "idle"}
		h.mu.Lock()
		h.onebot = append(h.onebot, f)
		h.mu.Unlock()
		return f, nil
	}
	t.Cleanup(func() {
		h.r.Close()
		h.r.Wait()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}
func (h *harness) start(t *testing.T) {
	t.Helper()
	h.r.Start()
	if h.cfg.Get().Channels.OneBot.Enabled && h.db.Initialized() {
		select {
		case <-h.ob().started:
		case <-time.After(2 * time.Second):
			t.Fatal("mock OneBot did not start")
		}
	}
}
func (h *harness) off() *fakeOfficial {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.official[len(h.official)-1]
}
func (h *harness) ob() *fakeOneBot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.onebot[len(h.onebot)-1]
}
func (h *harness) live(t *testing.T) {
	t.Helper()
	c := h.cfg.Get()
	c.Channels.Official.DryRun = false
	c.Channels.OneBot.DryRun = false
	if err := h.cfg.Save(c, config.Secrets{}); err != nil {
		t.Fatal(err)
	}
}
func (h *harness) bind(t *testing.T, channel, user string) string {
	t.Helper()
	ch, _ := channelConfig(h.cfg.Get(), channel)
	id := store.ID()
	if _, err := h.db.DB.Exec("INSERT INTO bindings(id,channel,bot_id,user_id,created_at) VALUES(?,?,?,?,?)", id, channel, botID(channel, ch), user, store.Now()); err != nil {
		t.Fatal(err)
	}
	return id
}
func (h *harness) event(t *testing.T, channel, messageID, user, text, group string, mentioned bool) incoming {
	t.Helper()
	i, _, err := h.r.current(channel)
	if err != nil {
		t.Fatal(err)
	}
	return incoming{i, channelEvent{ID: "event-" + messageID, BotID: i.botID, UserID: user, GroupID: group, Text: text, MessageID: messageID, IsGroup: group != "", Mentioned: mentioned}}
}
func (h *harness) push(t *testing.T, in incoming) {
	t.Helper()
	if !h.r.enqueue(in.instance, in.event) {
		t.Fatal("queue rejected test event")
	}
}
func (h *harness) nextTask(t *testing.T) store.Task {
	t.Helper()
	select {
	case task := <-h.engine.submitted:
		return task
	case <-time.After(3 * time.Second):
		t.Fatal("task not submitted")
		return store.Task{}
	}
}
func (h *harness) settled(t *testing.T) {
	t.Helper()
	eventually(t, func() bool { return len(h.r.receiveSlots) == 0 })
}
func (h *harness) finish(t *testing.T, task store.Task, output string) {
	t.Helper()
	task.Status, task.Output = "completed", output
	if err := h.db.UpdateTask(task.ID, task.Status, task.Output, ""); err != nil {
		t.Fatal(err)
	}
	h.r.TaskDone(task)
}
func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func noTask(t *testing.T, h *harness) {
	t.Helper()
	h.settled(t)
	select {
	case task := <-h.engine.submitted:
		t.Fatalf("unexpected task: %q", task.Input)
	default:
	}
}
func signingKey(secret string) ed25519.PrivateKey {
	for len(secret) < 32 {
		secret += secret
	}
	return ed25519.NewKeyFromSeed([]byte(secret[:32]))
}
func signedRequest(body, secret string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/callback/qq", strings.NewReader(body))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set(qqofficial.HeaderTimestamp, ts)
	req.Header.Set(qqofficial.HeaderSignature, hex.EncodeToString(ed25519.Sign(signingKey(secret), []byte(ts+body))))
	return req
}
func callbackBody(messageID, user, text string) string {
	b, _ := json.Marshal(map[string]any{"op": 0, "id": "event-" + messageID, "t": "C2C_MESSAGE_CREATE", "d": map[string]any{"id": messageID, "content": text, "author": map[string]any{"user_openid": user}}})
	return string(b)
}
func channelEvents(t *testing.T, db *store.Store) []map[string]string {
	t.Helper()
	events, err := db.Events(0, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, event := range events {
		if event.Kind == "channel" {
			var data map[string]string
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			out = append(out, data)
		}
	}
	return out
}
func hasStatus(t *testing.T, db *store.Store, status string) bool {
	t.Helper()
	for _, e := range channelEvents(t, db) {
		if e["status"] == status {
			return true
		}
	}
	return false
}

func TestCallbackVerifiesRawBodyChallengeAndPublicOrigin(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	body := " {\n\"op\":13, \"d\":{\"plain_token\":\"challenge-token\",\"event_ts\":\"1234567890\"}} "
	req := signedRequest(body, testOfficialSecret)
	req.Header.Set("Origin", "https://unrelated-browser.example")
	w := httptest.NewRecorder()
	h.r.Callback(w, req)
	if w.Code != 200 {
		t.Fatalf("challenge: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		PlainToken string `json:"plain_token"`
		Signature  string `json:"signature"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	sig, _ := hex.DecodeString(response.Signature)
	if response.PlainToken != "challenge-token" || !ed25519.Verify(signingKey(testOfficialSecret).Public().(ed25519.PublicKey), []byte("1234567890challenge-token"), sig) {
		t.Fatal("invalid challenge response")
	}
	if len(h.off().messages()) != 0 || len(h.ob().messages()) != 0 {
		t.Fatal("callback challenge sent a business message")
	}

	for _, tc := range []struct {
		name, body string
		status     int
		mutate     func(*http.Request)
	}{
		{"unsigned", `{ "op":0 }`, http.StatusUnauthorized, func(r *http.Request) { r.Header.Del(qqofficial.HeaderSignature) }},
		{"tampered", `{ "op":0 }`, http.StatusUnauthorized, func(r *http.Request) { r.Body = ioNopString(`{"op":1}`) }},
		{"stale", `{ "op":0 }`, http.StatusUnauthorized, func(r *http.Request) { r.Header.Set(qqofficial.HeaderTimestamp, "1") }},
		{"malformed", `{`, http.StatusBadRequest, nil},
		{"known length too large", strings.Repeat("x", qqofficial.MaxBodyBytes+1), http.StatusRequestEntityTooLarge, nil},
		{"chunked too large", strings.Repeat("x", qqofficial.MaxBodyBytes+1), http.StatusRequestEntityTooLarge, func(r *http.Request) { r.ContentLength = -1 }},
		{"unsupported event acknowledged", `{"op":0,"t":"IGNORED_EVENT","d":{}}`, http.StatusOK, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := signedRequest(tc.body, testOfficialSecret)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			w := httptest.NewRecorder()
			h.r.Callback(w, req)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if strings.Contains(w.Body.String(), testOfficialSecret) {
				t.Fatal("secret leaked")
			}
		})
	}
	w = httptest.NewRecorder()
	h.r.Callback(w, httptest.NewRequest(http.MethodGet, "/api/callback/qq", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}

type stringBody struct{ *strings.Reader }

func (stringBody) Close() error       { return nil }
func ioNopString(s string) stringBody { return stringBody{strings.NewReader(s)} }

func TestCallbackInitializationDisableAndCredentialChangeFailClosed(t *testing.T) {
	h := newHarness(t)
	if err := h.db.Set("initialized", "false"); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	call := func() int {
		w := httptest.NewRecorder()
		h.r.Callback(w, signedRequest(`{"op":0}`, testOfficialSecret))
		return w.Code
	}
	if call() != 403 {
		t.Fatal("uninitialized callback accepted")
	}
	if err := h.db.Set("initialized", "true"); err != nil {
		t.Fatal(err)
	}
	h.r.Reload()
	if call() != 200 {
		t.Fatal("initialized callback not accepted")
	}
	if err := h.cfg.Save(h.cfg.Get(), config.Secrets{OfficialSecret: "new-official-secret-value"}); err != nil {
		t.Fatal(err)
	}
	if call() != 403 {
		t.Fatal("old credentials accepted before Reload")
	}
	h.r.Reload()
	if call() != 401 {
		t.Fatal("old signing secret survived Reload")
	}
	c := h.cfg.Get()
	c.Channels.Official.Enabled = false
	if err := h.cfg.Save(c, config.Secrets{}); err != nil {
		t.Fatal(err)
	}
	if call() != 403 {
		t.Fatal("disabled callback accepted before Reload")
	}
}

func TestCallbackACKIsIndependentOfDatabaseAndQueueIsBounded(t *testing.T) {
	h := newHarness(t)
	h.r.window = time.Hour
	h.start(t)
	h.bind(t, "official", "owner")
	tx, err := h.db.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.r.Callback(w, signedRequest(callbackBody("first", "owner", "inspect"), testOfficialSecret))
		finished <- w
	}()
	select {
	case w := <-finished:
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"op":12,"d":0}` {
			t.Fatalf("ACK: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(500 * time.Millisecond):
		tx.Rollback()
		t.Fatal("callback waited on the database")
	}
	for j := 1; j < queueLimit; j++ {
		h.push(t, h.event(t, "official", fmt.Sprint(j), "owner", "message", "", false))
	}
	if len(h.r.receiveSlots) != queueLimit {
		t.Fatal("receive budget not reserved")
	}
	w := httptest.NewRecorder()
	h.r.Callback(w, signedRequest(callbackBody("overflow", "owner", "inspect"), testOfficialSecret))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("full queue accepted callback: %d", w.Code)
	}
	h.r.Close()
	tx.Rollback()
	h.r.Wait()
	if len(h.r.receiveSlots) != 0 || len(h.r.receive) != 0 {
		t.Fatal("Close leaked receive reservations")
	}
}

func TestPairingIsOneShotChannelAndBotScopedAndWebApproved(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	value, err := h.r.Pair("official")
	if err != nil {
		t.Fatal(err)
	}
	pair := value.(Pairing)
	decoded, err := hex.DecodeString(pair.Code)
	if err != nil || len(decoded) < 16 || pair.ID == "" {
		t.Fatal("pair code has less than 128 bits")
	}
	if h.r.ApprovePair(pair.ID) == nil {
		t.Fatal("approved without candidate")
	}
	h.push(t, h.event(t, "onebot", "1", "20001", "/bind "+pair.Code, "", false))
	noTask(t, h)
	var candidate string
	if err := h.db.DB.QueryRow("SELECT user_id FROM pairings WHERE id=?", pair.ID).Scan(&candidate); err != nil || candidate != "" {
		t.Fatal("cross-channel pair claimed")
	}
	wrongBot := h.event(t, "official", "wrong-bot", "20001", "/bind "+pair.Code, "", false)
	wrongBot.event.BotID = "other-bot"
	h.push(t, wrongBot)
	noTask(t, h)
	h.push(t, h.event(t, "official", "2", "20001", "/bind "+pair.Code, "", false))
	noTask(t, h)
	h.push(t, h.event(t, "official", "3", "20002", "/bind "+pair.Code, "", false))
	noTask(t, h)
	if err := h.db.DB.QueryRow("SELECT user_id FROM pairings WHERE id=?", pair.ID).Scan(&candidate); err != nil || candidate != "20001" {
		t.Fatal("candidate was overwritten")
	}
	h.push(t, h.event(t, "official", "4", "20001", "not authorized yet", "", false))
	noTask(t, h)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for j := 0; j < 2; j++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- h.r.ApprovePair(pair.ID) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent approvals succeeded %d times", successes)
	}
	bindings, err := h.r.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if got := bindings["bindings"].([]Binding); len(got) != 1 || got[0].Channel != "official" || got[0].BotID != "bot-official" || got[0].UserID != "20001" {
		t.Fatalf("wrong bindings: %#v", got)
	}
	if len(bindings["pending"].([]Pairing)) != 0 {
		t.Fatal("approved pair not consumed")
	}
	h.push(t, h.event(t, "onebot", "5", "20001", "cross-channel access", "", false))
	noTask(t, h)
	h.push(t, h.event(t, "official", "6", "20001", "authorized request", "", false))
	task := h.nextTask(t)
	if task.Input != "authorized request" || !task.DryRun {
		t.Fatal("authorized dry-run request not routed")
	}
	for _, event := range channelEvents(t, h.db) {
		for _, field := range event {
			if strings.Contains(field, pair.Code) {
				t.Fatal("pair code leaked to timeline")
			}
		}
	}
}

func TestExpiredPairAndChangedBotCannotApprove(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	value, err := h.r.Pair("onebot")
	if err != nil {
		t.Fatal(err)
	}
	p := value.(Pairing)
	if _, err := h.db.DB.Exec("UPDATE pairings SET expires_at=?,user_id='20001' WHERE id=?", time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), p.ID); err != nil {
		t.Fatal(err)
	}
	if h.r.ApprovePair(p.ID) == nil {
		t.Fatal("expired pair approved")
	}
	value, err = h.r.Pair("onebot")
	if err != nil {
		t.Fatal(err)
	}
	p = value.(Pairing)
	h.push(t, h.event(t, "onebot", "pair", "20001", "/bind "+p.Code, "", false))
	noTask(t, h)
	c := h.cfg.Get()
	c.Channels.OneBot.BotID = "10001"
	if err := h.cfg.Save(c, config.Secrets{}); err != nil {
		t.Fatal(err)
	}
	h.r.Reload()
	if h.r.ApprovePair(p.ID) == nil {
		t.Fatal("pair approved for changed bot")
	}
}

func TestReceiveGuardsAndStrictGroupTrigger(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.bind(t, "onebot", "20001")
	base := h.event(t, "onebot", "base", "20001", "inspect", "", false)
	cases := []func(*channelEvent){
		func(e *channelEvent) { e.ID = "" }, func(e *channelEvent) { e.MessageID = "" },
		func(e *channelEvent) { e.UserID = e.BotID }, func(e *channelEvent) { e.BotID = "10001" },
		func(e *channelEvent) { e.UserID = "unbound" }, func(e *channelEvent) { e.UserID = "bad:user" },
		func(e *channelEvent) { e.Text = strings.Repeat("x", inputLimit+1) },
		func(e *channelEvent) { e.GroupID = "30000" },
		func(e *channelEvent) { e.IsGroup = true; e.GroupID = "99999"; e.Mentioned = true },
		func(e *channelEvent) { e.IsGroup = true; e.GroupID = "30000"; e.Text = "/opsx inspect" },
		func(e *channelEvent) { e.IsGroup = true; e.GroupID = "30000"; e.Text = "ordinary group chat" },
		func(e *channelEvent) { e.IsGroup = true; e.GroupID = "30000"; e.Text = "quoted @10000 inspect" },
		func(e *channelEvent) { e.IsGroup = true; e.GroupID = "30000"; e.Mentioned = true; e.UserID = "20002" },
	}
	for j, change := range cases {
		in := base
		in.event.ID = fmt.Sprint("event-", j)
		in.event.MessageID = fmt.Sprint(j)
		change(&in.event)
		h.push(t, in)
	}
	noTask(t, h)
	h.push(t, h.event(t, "onebot", "valid-prefix", "20001", "/ops inspect", "30000", false))
	task := h.nextTask(t)
	if task.Input != "inspect" {
		t.Fatal(task.Input)
	}
	s, err := h.db.Session(task.SessionID)
	if err != nil || s.Scope != "group:onebot:10000:30000" || s.Channel != "onebot:10000" {
		t.Fatalf("incorrect group session: %#v %v", s, err)
	}
	h.push(t, h.event(t, "onebot", "valid-mention", "20001", "another question", "30000", true))
	task2 := h.nextTask(t)
	if task2.SessionID != task.SessionID {
		t.Fatal("same sender conversation did not reuse session")
	}
}

func TestDedupMergesOnlySameSenderAndConversation(t *testing.T) {
	h := newHarness(t)
	h.r.window = 150 * time.Millisecond
	h.start(t)
	h.bind(t, "official", "20001")
	h.bind(t, "official", "20002")
	h.bind(t, "onebot", "20001")
	h.push(t, h.event(t, "official", "same-id", "20001", "first", "", false))
	h.push(t, h.event(t, "official", "same-id", "20001", "duplicate ignored", "", false))
	h.push(t, h.event(t, "official", "second", "20001", "second", "", false))
	h.push(t, h.event(t, "onebot", "same-id", "20001", "other channel", "", false))
	h.push(t, h.event(t, "official", "group-one", "20001", "group user one", "group-a", true))
	h.push(t, h.event(t, "official", "group-two", "20002", "group user two", "group-a", true))
	h.push(t, h.event(t, "official", "same-id", "20001", "other group", "group-b", true))
	byInput := map[string]store.Session{}
	for j := 0; j < 5; j++ {
		task := h.nextTask(t)
		s, err := h.db.Session(task.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		byInput[task.Input] = s
		if !task.DryRun {
			t.Fatal("default channel task was live")
		}
	}
	noTask(t, h)
	if len(byInput) != 5 || byInput["first\nsecond"].Scope != "owner" || byInput["other channel"].Scope != "owner" {
		t.Fatalf("wrong merge: %#v", byInput)
	}
	if byInput["first\nsecond"].ID == byInput["other channel"].ID || byInput["first\nsecond"].Channel == byInput["other channel"].Channel {
		t.Fatal("cross-channel history shared")
	}
	if byInput["group user one"].ID == byInput["group user two"].ID {
		t.Fatal("different sender authorizations merged")
	}
	if byInput["group user one"].Scope == byInput["other group"].Scope || byInput["group user one"].Scope == "owner" {
		t.Fatal("group memory scopes were shared")
	}
}

func TestMergeCapsEightMessagesAnd12000Runes(t *testing.T) {
	h := newHarness(t)
	h.r.window = 100 * time.Millisecond
	h.start(t)
	h.bind(t, "official", "owner")
	for j := 0; j < 9; j++ {
		h.push(t, h.event(t, "official", fmt.Sprint(j), "owner", fmt.Sprint("line", j), "", false))
	}
	first, second := h.nextTask(t), h.nextTask(t)
	if len(strings.Split(first.Input, "\n")) != 8 || second.Input != "line8" {
		t.Fatal("batch exceeded 8 messages")
	}
	h.push(t, h.event(t, "official", "large1", "owner", strings.Repeat("界", 6500), "", false))
	h.push(t, h.event(t, "official", "large2", "owner", strings.Repeat("文", 5500), "", false))
	first, second = h.nextTask(t), h.nextTask(t)
	if utf8.RuneCountInString(first.Input) != 6500 || utf8.RuneCountInString(second.Input) != 5500 {
		t.Fatal("rune budget merged beyond limit or truncated input")
	}
}

func TestDryRunNeverSendsAndPreviewRedacts(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.bind(t, "official", "owner")
	h.push(t, h.event(t, "official", "msg", "owner", "inspect", "", false))
	task := h.nextTask(t)
	if !task.DryRun {
		t.Fatal("dry-run not passed to engine")
	}
	h.finish(t, task, "result "+testOfficialSecret+" password=supersecret")
	eventually(t, func() bool { return hasStatus(t, h.db, "dry_run") })
	if len(h.off().messages()) != 0 || len(h.ob().messages()) != 0 {
		t.Fatal("dry-run sent a message")
	}
	for _, e := range channelEvents(t, h.db) {
		if strings.Contains(e["text"], testOfficialSecret) || strings.Contains(e["text"], "supersecret") {
			t.Fatal("preview leaked secrets")
		}
	}
	h.r.tasksMu.Lock()
	n := len(h.r.tasks)
	h.r.tasksMu.Unlock()
	if n != 0 {
		t.Fatal("done task correlation retained")
	}
}

func TestFinalReplyIsSingleBoundedRedactedAndGroupSafe(t *testing.T) {
	h := newHarness(t)
	h.live(t)
	h.start(t)
	h.bind(t, "official", "owner")
	h.push(t, h.event(t, "official", "private", "owner", "inspect", "", false))
	task := h.nextTask(t)
	if len(h.off().messages()) != 0 {
		t.Fatal("progress message sent")
	}
	task.Status, task.Output = "completed", testOfficialSecret+" "+strings.Repeat("结果", 400)
	h.r.TaskDone(task)
	h.r.TaskDone(task)
	eventually(t, func() bool { return len(h.off().messages()) == 1 })
	sent := h.off().messages()[0]
	if sent.event.MessageID != "private" || sent.event.IsGroup || strings.Contains(sent.text, testOfficialSecret) || utf8.RuneCountInString(sent.text) > replyLimit {
		t.Fatalf("unsafe reply: %#v", sent)
	}
	h.push(t, h.event(t, "official", "group", "owner", "inspect", "group-a", true))
	task = h.nextTask(t)
	h.finish(t, task, "private host information: 10.20.30.40 /etc/passwd")
	eventually(t, func() bool { return len(h.off().messages()) == 2 })
	sent = h.off().messages()[1]
	if !sent.event.IsGroup || strings.Contains(sent.text, "10.20") || strings.Contains(sent.text, "/etc") || !strings.Contains(sent.text, "未执行本地工具") {
		t.Fatalf("group host disclosure: %#v", sent)
	}
	h.off().mu.Lock()
	h.off().sendErr = errors.New("remote secret: " + testOfficialSecret)
	h.off().mu.Unlock()
	h.push(t, h.event(t, "official", "failed", "owner", "inspect", "", false))
	task = h.nextTask(t)
	h.finish(t, task, "answer")
	eventually(t, func() bool { return hasStatus(t, h.db, "reply_failed") })
	if len(h.off().messages()) != 3 {
		t.Fatal("failed reply retried")
	}
	for _, e := range channelEvents(t, h.db) {
		if strings.Contains(e["message"], testOfficialSecret) {
			t.Fatal("transport error secret leaked")
		}
	}
}

func TestAuthorityRevocationAndRebindingCannotReviveQueuedWork(t *testing.T) {
	h := newHarness(t)
	h.live(t)
	h.start(t)
	binding := h.bind(t, "official", "owner")
	h.push(t, h.event(t, "official", "old", "owner", "inspect", "", false))
	task := h.nextTask(t)
	if err := h.r.AuthorizeTask(task); err != nil {
		t.Fatal(err)
	}
	h.r.tasksMu.Lock()
	origin := h.r.tasks[task.ID]
	h.r.tasksMu.Unlock()
	if err := h.r.RemoveBinding(binding); err != nil {
		t.Fatal(err)
	}
	if h.r.AuthorizeTask(task) == nil {
		t.Fatal("revoked task retained engine authority")
	}
	select {
	case id := <-h.engine.cancelled:
		if id != task.ID {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("queued task not cancelled")
	}
	h.bind(t, "official", "owner")
	if h.r.AuthorizeTask(task) == nil {
		t.Fatal("new binding revived old task")
	}
	task.Status = "completed"
	h.r.deliver(outgoing{origin: origin, task: task, text: "must not send"})
	if len(h.off().messages()) != 0 {
		t.Fatal("queued reply bypassed latest binding")
	}
	h.push(t, h.event(t, "official", "new", "owner", "new request", "", false))
	newer := h.nextTask(t)
	if newer.SessionID == task.SessionID {
		t.Fatal("re-pair reused old authority history")
	}
}

func TestBufferedTaskRechecksBindingAndCurrentGroupPolicy(t *testing.T) {
	h := newHarness(t)
	h.r.window = 200 * time.Millisecond
	h.start(t)
	binding := h.bind(t, "onebot", "20001")
	h.push(t, h.event(t, "onebot", "buffered", "20001", "inspect", "30000", true))
	// Seen is written before the batch is held, so waiting for it proves this
	// is a queued/merged event rather than merely an unread receive entry.
	eventually(t, func() bool {
		var n int
		_ = h.db.DB.QueryRow("SELECT count(*) FROM channel_seen").Scan(&n)
		return n == 1
	})
	if err := h.r.RemoveBinding(binding); err != nil {
		t.Fatal(err)
	}
	noTask(t, h)
	h.bind(t, "onebot", "20001")
	h.push(t, h.event(t, "onebot", "submitted", "20001", "inspect", "30000", true))
	task := h.nextTask(t)
	c := h.cfg.Get()
	c.Channels.OneBot.Groups = nil
	if err := h.cfg.Save(c, config.Secrets{}); err != nil {
		t.Fatal(err)
	}
	if h.r.AuthorizeTask(task) == nil {
		t.Fatal("group removal took effect only after Reload")
	}
	h.r.Reload()
	if h.r.AuthorizeTask(task) == nil {
		t.Fatal("changed instance retained authority")
	}
	web, err := h.db.CreateSession("web", "owner", "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.AuthorizeTask(store.Task{SessionID: web.ID}); err != nil {
		t.Fatal("web owner task blocked", err)
	}
	forged, err := h.db.CreateSession("forged", "owner", "official:bot-official")
	if err != nil {
		t.Fatal(err)
	}
	if h.r.AuthorizeTask(store.Task{SessionID: forged.ID}) == nil {
		t.Fatal("channel session without route authority accepted")
	}
}

func TestDoneAndAuthorizeCanRunBeforeSubmitReturns(t *testing.T) {
	h := newHarness(t)
	h.live(t)
	authorized := make(chan error, 1)
	h.engine.onSubmit = func(task store.Task) {
		authorized <- h.r.AuthorizeTask(task)
		entered := make(chan struct{})
		go func() { close(entered); task.Status, task.Output = "completed", "fast answer"; h.r.TaskDone(task) }()
		<-entered
	}
	h.start(t)
	h.bind(t, "onebot", "20001")
	h.push(t, h.event(t, "onebot", "fast", "20001", "inspect", "", false))
	h.nextTask(t)
	select {
	case err := <-authorized:
		if err != nil {
			t.Fatal("pre-registration authority failed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("authority callback deadlocked")
	}
	eventually(t, func() bool { return len(h.ob().messages()) == 1 })
	if h.ob().messages()[0].text != "fast answer" {
		t.Fatal("early Done reply lost")
	}
}

func TestReloadIsIdempotentCancelsOldConnectionsAndDoesNotReplay(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.bind(t, "onebot", "20001")
	first := h.ob()
	oldOfficial := h.off()
	h.r.Start()
	h.r.Reload()
	if h.ob() != first || h.off() != oldOfficial {
		t.Fatal("unchanged reload rebuilt clients")
	}
	c := h.cfg.Get()
	c.Persona.Name = "new persona"
	if err := h.cfg.Save(c, config.Secrets{}); err != nil {
		t.Fatal(err)
	}
	h.r.Reload()
	if h.ob() != first || h.off() != oldOfficial {
		t.Fatal("unrelated config restarted channels")
	}
	h.push(t, h.event(t, "onebot", "old", "20001", "inspect", "", false))
	task := h.nextTask(t)
	if err := h.cfg.Save(h.cfg.Get(), config.Secrets{OneBotToken: "replacement-onebot-token"}); err != nil {
		t.Fatal(err)
	}
	h.r.Reload()
	if h.ob() == first || h.off() != oldOfficial {
		t.Fatal("credential reload rebuilt wrong instances")
	}
	select {
	case <-first.stopped:
	default:
		t.Fatal("old connection was not stopped before replacement")
	}
	if h.r.AuthorizeTask(task) == nil {
		t.Fatal("old task accepted after credential change")
	}
	h.finish(t, task, "do not replay")
	if len(h.ob().messages()) != 0 {
		t.Fatal("old completion replayed")
	}
	c = h.cfg.Get()
	c.Channels.OneBot.Enabled = false
	if err := h.cfg.Save(c, config.Secrets{}); err != nil {
		t.Fatal(err)
	}
	last := h.ob()
	h.r.Reload()
	select {
	case <-last.stopped:
	default:
		t.Fatal("disabled connection still running")
	}
	status := h.r.Status().(map[string]channelStatus)
	if status["onebot"].Enabled || status["onebot"].Status != "disabled" || !status["onebot"].DryRun {
		t.Fatalf("wrong status: %#v", status)
	}
	h.r.Close()
	h.r.Close()
	h.r.Wait()
	h.r.Start()
	if _, _, err := h.r.current("official"); err == nil {
		t.Fatal("closed router restarted")
	}
}

func TestCheckNeverSendsAndCanTestDisabledChannel(t *testing.T) {
	h := newHarness(t)
	c := h.cfg.Get()
	c.Channels.Official.Enabled = false
	c.Channels.OneBot.Enabled = false
	if err := h.cfg.Save(c, config.Secrets{}); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	for _, channel := range []string{"official", "onebot"} {
		if err := h.r.Check(context.Background(), channel); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.off().messages()) != 0 || len(h.ob().messages()) != 0 {
		t.Fatal("connectivity check sent a business message")
	}
	h.ob().mu.Lock()
	runs, checks := h.ob().runs, h.ob().checks
	h.ob().mu.Unlock()
	if runs != 0 || checks != 1 {
		t.Fatal("connectivity check started a receiver")
	}
	if h.r.Check(context.Background(), "unknown") == nil {
		t.Fatal("unknown channel accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.r.Check(ctx, "official") == nil {
		t.Fatal("cancelled connection check succeeded")
	}
}

func TestNotifyRequiresBindingAndDryRunAndOfficialIsWebOnly(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.bind(t, "official", "owner")
	h.bind(t, "onebot", "20001")
	if h.r.Notify(context.Background(), "onebot", "unbound", "notification") == nil {
		t.Fatal("unbound notification succeeded")
	}
	if err := h.r.Notify(context.Background(), "onebot", "20001", "notification"); !errors.Is(err, ErrDryRun) {
		t.Fatal(err)
	}
	if len(h.ob().messages()) != 0 {
		t.Fatal("dry-run watch notification sent")
	}
	h.live(t)
	h.r.Reload()
	if err := h.r.Notify(context.Background(), "official", "owner", testOfficialSecret+" notification"); !errors.Is(err, ErrActiveUnsupported) {
		t.Fatal(err)
	}
	if len(h.off().messages()) != 0 {
		t.Fatal("official passive Send abused for active notification")
	}
	var state, message string
	if err := h.db.DB.QueryRow("SELECT state,message FROM notifications WHERE metric='channel'").Scan(&state, &message); err != nil || state != "web_only" || strings.Contains(message, testOfficialSecret) {
		t.Fatalf("web inbox: %q %q %v", state, message, err)
	}
	if err := h.r.Notify(context.Background(), "onebot", "20001", "notice "+testOfficialSecret); err != nil {
		t.Fatal(err)
	}
	sent := h.ob().messages()
	if len(sent) != 1 || sent[0].event.MessageID != "" || sent[0].event.IsGroup || sent[0].event.UserID != "20001" || strings.Contains(sent[0].text, testOfficialSecret) {
		t.Fatalf("unsafe active notification: %#v", sent)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.r.Notify(ctx, "onebot", "20001", "cancelled") == nil {
		t.Fatal("cancelled notification sent")
	}
	if len(h.ob().messages()) != 1 {
		t.Fatal("cancelled notification changed send count")
	}
}

func TestCancelledTaskCorrelationPrunedAndRestartCannotReplay(t *testing.T) {
	h := newHarness(t)
	h.live(t)
	h.start(t)
	h.bind(t, "official", "owner")
	h.push(t, h.event(t, "official", "old", "owner", "inspect", "", false))
	task := h.nextTask(t)
	if err := h.engine.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	h.r.pruneTasks()
	h.r.tasksMu.Lock()
	n := len(h.r.tasks)
	h.r.tasksMu.Unlock()
	if n != 0 {
		t.Fatal("queued cancellation leaked correlation")
	}
	h.r.Close()
	h.r.Wait()
	r := New(context.Background(), h.db, h.cfg, nil)
	r.engine = h.engine
	r.newOfficial = h.r.newOfficial
	r.newOneBot = h.r.newOneBot
	t.Cleanup(func() { r.Close(); r.Wait() })
	r.Start()
	if r.AuthorizeTask(task) == nil {
		t.Fatal("restart revived authority of old task")
	}
	task.Status, task.Output = "completed", "do not replay"
	r.TaskDone(task)
	if len(h.off().messages()) != 0 {
		t.Fatal("restart replayed a reply")
	}
}
