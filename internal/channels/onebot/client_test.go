package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const testBotID = "9007199254740993"

type testRequest struct {
	Action string                     `json:"action"`
	Params map[string]json.RawMessage `json:"params"`
	Echo   string                     `json:"echo"`
}

func wsServer(t *testing.T, handle func(context.Context, *websocket.Conn, *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(MaxBodyBytes)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		handle(ctx, conn, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func newWSClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c, err := New(Options{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Token: "mock-access-token", BotID: testBotID})
	if err != nil {
		t.Fatal(err)
	}
	c.backoffMin, c.backoffMax = 5*time.Millisecond, 20*time.Millisecond
	return c
}

func readRequest(t *testing.T, ctx context.Context, conn *websocket.Conn) (testRequest, bool) {
	t.Helper()
	var req testRequest
	if err := wsjson.Read(ctx, conn, &req); err != nil {
		return req, false
	}
	if req.Echo == "" {
		t.Error("missing echo")
		return req, false
	}
	return req, true
}

func reply(ctx context.Context, conn *websocket.Conn, echo string, data any) error {
	return wsjson.Write(ctx, conn, map[string]any{"echo": echo, "status": "ok", "retcode": 0, "data": data})
}

func login(t *testing.T, ctx context.Context, conn *websocket.Conn, botID string) bool {
	t.Helper()
	req, ok := readRequest(t, ctx, conn)
	if !ok {
		return false
	}
	if req.Action != "get_login_info" {
		t.Errorf("business request before identity check: %s", req.Action)
		return false
	}
	if err := reply(ctx, conn, req.Echo, map[string]any{"user_id": json.Number(botID), "nickname": "mock bot"}); err != nil {
		t.Errorf("login reply: %v", err)
		return false
	}
	return true
}

func writeEvent(ctx context.Context, conn *websocket.Conn, id int64, self string) error {
	return wsjson.Write(ctx, conn, map[string]any{
		"post_type": "message", "message_type": "private",
		"self_id": json.Number(self), "user_id": json.Number("9007199254740995"),
		"message_id": id, "message": "/status",
	})
}

func awaitValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for mock connection")
		var zero T
		return zero
	}
}

func waitConnected(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Status() == "connected" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("not connected: %s", c.Status())
}

func TestNewValidationAndNoAutomaticNetwork(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	c := newWSClient(t, server)
	if c.Status() != "idle" || calls.Load() != 0 {
		t.Fatal("New performed network I/O")
	}
	if err := c.Send(context.Background(), Event{UserID: "123"}, "text"); !errors.Is(err, ErrNotConnected) || calls.Load() != 0 {
		t.Fatalf("Send must not connect: %v", err)
	}
	for _, endpoint := range []string{"http://localhost:3000", "ws://u:token@localhost/", "ws://localhost/?access_token=secret", "ws://localhost/#x", "ws://localhost/api", "ws://localhost/event/"} {
		if _, err := New(Options{URL: endpoint, BotID: "123"}); err == nil {
			t.Fatalf("unsafe/unsupported URL accepted: %s", endpoint)
		}
	}
	for _, id := range []string{"", "01", "-1", "0", "1e3", "9223372036854775808"} {
		if _, err := New(Options{URL: "ws://localhost/", BotID: id}); err == nil {
			t.Fatalf("bad BotID accepted: %s", id)
		}
	}
}

func TestCheckHeadersIdentityAndNoBusinessMessages(t *testing.T) {
	var requests atomic.Int64
	done := make(chan struct{})
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		defer close(done)
		if r.Header.Get("Authorization") != "Bearer mock-access-token" || r.URL.RawQuery != "" {
			t.Error("token must be in Authorization only")
		}
		if !login(t, ctx, conn, testBotID) {
			return
		}
		requests.Add(1)
		if _, ok := readRequest(t, ctx, conn); ok {
			t.Error("Check sent another API request")
		}
	})
	c := newWSClient(t, server)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitValue(t, done)
	if requests.Load() != 1 || c.Status() != "check_ok" {
		t.Fatalf("Check did not cleanly verify: %d %s", requests.Load(), c.Status())
	}
	if err := c.Send(context.Background(), Event{UserID: "123"}, "text"); !errors.Is(err, ErrNotConnected) {
		t.Fatal("Check must not publish a business connection")
	}
}

func TestWrongBotFailsClosedWithoutReconnectOrHandler(t *testing.T) {
	var connections, handled atomic.Int64
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		connections.Add(1)
		_ = writeEvent(ctx, conn, 1, testBotID) // Before identity verification.
		login(t, ctx, conn, "999")
		_ = writeEvent(ctx, conn, 2, testBotID)
		_, _ = readRequest(t, ctx, conn)
	})
	c := newWSClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := c.Run(ctx, func(Event) { handled.Add(1) })
	if !errors.Is(err, ErrBotIDMismatch) || connections.Load() != 1 || handled.Load() != 0 || c.Status() != "identity_mismatch" {
		t.Fatalf("identity fail-closed: %v connections=%d handled=%d state=%s", err, connections.Load(), handled.Load(), c.Status())
	}
}

func TestEventsAfterLoginAndHandlerCanSend(t *testing.T) {
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_ = writeEvent(ctx, conn, 1, testBotID) // Must be ignored, not buffered.
		if !login(t, ctx, conn, testBotID) {
			return
		}
		// No scheduling delay: tests the login-response/event-frame boundary.
		_ = writeEvent(ctx, conn, 2, testBotID)
		req, ok := readRequest(t, ctx, conn)
		if !ok {
			return
		}
		if req.Action != "send_private_msg" || string(req.Params["user_id"]) != "9007199254740995" || string(req.Params["auto_escape"]) != "true" {
			t.Error("incorrect text send action/target/escaping")
		}
		var segments []textSegment
		if json.Unmarshal(req.Params["message"], &segments) != nil || len(segments) != 1 || segments[0].Type != "text" || segments[0].Data.Text != "[CQ:image,file=file:///do-not-open]" {
			t.Errorf("CQ payload was not sent as literal text: %s", req.Params["message"])
		}
		_ = reply(ctx, conn, "wrong-echo", map[string]any{"message_id": 99})
		_ = reply(ctx, conn, req.Echo, map[string]any{"message_id": 3})
		_, _ = readRequest(t, ctx, conn)
	})
	c := newWSClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	events := make(chan Event, 2)
	go func() {
		result <- c.Run(ctx, func(e Event) {
			events <- e
			if err := c.Send(ctx, e, "[CQ:image,file=file:///do-not-open]"); err != nil {
				t.Errorf("send from handler: %v", err)
			}
			cancel()
		})
	}()
	if e := awaitValue(t, events); e.MessageID != "2" || e.UserID != "9007199254740995" {
		t.Fatalf("wrong event after verified login: %#v", e)
	}
	if err := awaitValue(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancellation: %v", err)
	}
	if len(events) != 0 {
		t.Fatal("unverified event dispatched")
	}
}

func TestConcurrentSendOutOfOrderEchoAndAPIError(t *testing.T) {
	const count = 16
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if !login(t, ctx, conn, testBotID) {
			return
		}
		requests := make([]testRequest, 0, count)
		seen := make(map[string]bool)
		for i := 0; i < count; i++ {
			req, ok := readRequest(t, ctx, conn)
			if !ok {
				return
			}
			if seen[req.Echo] {
				t.Error("reused echo")
			}
			seen[req.Echo] = true
			requests = append(requests, req)
		}
		for i := len(requests) - 1; i >= 0; i-- {
			var segments []textSegment
			json.Unmarshal(requests[i].Params["message"], &segments)
			if len(segments) != 1 {
				t.Error("not text")
				return
			}
			if segments[0].Data.Text == "fail" {
				_ = wsjson.Write(ctx, conn, map[string]any{"echo": requests[i].Echo, "status": "failed", "retcode": 100, "message": "mock-access-token"})
			} else {
				_ = reply(ctx, conn, requests[i].Echo, map[string]any{"message_id": i + 1})
			}
		}
		_, _ = readRequest(t, ctx, conn)
	})
	c := newWSClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- c.Run(ctx, func(Event) {}) }()
	waitConnected(t, c)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			text := fmt.Sprintf("text-%d", i)
			if i == 3 {
				text = "fail"
			}
			err := c.Send(ctx, Event{IsGroup: true, GroupID: "9007199254740997", BotID: testBotID}, text)
			if i == 3 {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || strings.Contains(err.Error(), "mock-access-token") {
					t.Errorf("wrong API error: %v", err)
				}
			} else if err != nil {
				t.Errorf("echo miscorrelation: %v", err)
			}
		}(i)
	}
	wg.Wait()
	cancel()
	awaitValue(t, run)
}

func TestReconnectRevalidatesIdentity(t *testing.T) {
	var connections atomic.Int64
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		n := connections.Add(1)
		if n == 1 {
			login(t, ctx, conn, testBotID)
			return // Force a transport loss, not a business-send retry.
		}
		login(t, ctx, conn, "999")
		_, _ = readRequest(t, ctx, conn)
	})
	c := newWSClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Run(ctx, func(Event) { t.Error("unexpected handler") }); !errors.Is(err, ErrBotIDMismatch) || connections.Load() != 2 {
		t.Fatalf("reconnect failed to recheck identity: %v (%d)", err, connections.Load())
	}
}

func TestCancellationClearsPendingAndClosesSocket(t *testing.T) {
	gotSend := make(chan struct{})
	closed := make(chan struct{})
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		defer close(closed)
		if !login(t, ctx, conn, testBotID) {
			return
		}
		if _, ok := readRequest(t, ctx, conn); !ok {
			return
		}
		close(gotSend)
		_, _ = readRequest(t, ctx, conn) // No reply; cancellation must release client.
	})
	c := newWSClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := make(chan error, 1)
	go func() { run <- c.Run(ctx, func(Event) {}) }()
	waitConnected(t, c)
	c.mu.Lock()
	s := c.active
	c.mu.Unlock()
	sent := make(chan error, 1)
	go func() { sent <- c.Send(context.Background(), Event{UserID: "456"}, "text") }()
	awaitValue(t, gotSend)
	cancel()
	if err := awaitValue(t, sent); err == nil {
		t.Fatal("pending send falsely succeeded")
	}
	awaitValue(t, run)
	awaitValue(t, closed)
	s.mu.Lock()
	count := len(s.pending)
	s.mu.Unlock()
	if count != 0 || c.Status() != "stopped" {
		t.Fatalf("pending state leaked: %d %s", count, c.Status())
	}
}

func TestCheckTimeoutAndRedirectSafety(t *testing.T) {
	var leaked atomic.Int64
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer dest.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 307) }))
	defer redirect.Close()
	if err := newWSClient(t, redirect).Check(context.Background()); err == nil || leaked.Load() != 0 {
		t.Fatalf("credential redirect: %v", err)
	}
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_, _ = readRequest(t, ctx, conn)
		_, _ = readRequest(t, ctx, conn)
	})
	c := newWSClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := c.Check(ctx); err == nil || time.Since(started) > time.Second {
		t.Fatalf("unbounded check: %v", err)
	}
}

func TestAccountSwapInEventFailsClosed(t *testing.T) {
	server := wsServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if !login(t, ctx, conn, testBotID) {
			return
		}
		_ = writeEvent(ctx, conn, 1, "999")
		_, _ = readRequest(t, ctx, conn)
	})
	c := newWSClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Run(ctx, func(Event) { t.Error("wrong account event dispatched") }); !errors.Is(err, ErrBotIDMismatch) {
		t.Fatal(err)
	}
}
