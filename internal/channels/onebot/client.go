// Package onebot implements the OneBot v11 forward WebSocket transport.
// New is offline. Only Run and Check connect; only an explicit Send transmits
// business messages. The application owns disabled/dry-run and deduplication.
package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	MaxBodyBytes    = 1 << 20
	MaxTextBytes    = 16 << 10
	maxPending      = 128
	maxQueuedEvents = 64
	requestTimeout  = 10 * time.Second
)

var (
	ErrBotIDMismatch   = errors.New("onebot: BotID mismatch; connection refused")
	ErrNotConnected    = errors.New("onebot: no verified connection")
	ErrDisconnected    = errors.New("onebot: connection closed; delivery state may be unknown")
	ErrAlreadyRunning  = errors.New("onebot: a receive loop is already running")
	ErrCheckInProgress = errors.New("onebot: a connection check is already running")
	ErrProtocol        = errors.New("onebot: invalid or unsupported protocol payload")
	ErrBackpressure    = errors.New("onebot: event queue full; connection closed")
	ErrTooManyPending  = errors.New("onebot: too many pending requests")
)

type Options struct {
	URL   string
	Token string
	BotID string
}

type Event struct {
	ID        string
	BotID     string
	UserID    string
	GroupID   string
	Text      string
	MessageID string
	IsGroup   bool
	Mentioned bool // A real leading at/CQ-at segment targeting this BotID.
}

// APIError excludes server-supplied human-readable messages and echoes.
// Even status=async does not confirm delivery, so it is not treated as success.
type APIError struct {
	RetCode int64
}

func (e *APIError) Error() string {
	return fmt.Sprintf("onebot: API did not confirm success (retcode %d)", e.RetCode)
}

type HandshakeError struct {
	HTTPStatus int
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("onebot: WebSocket handshake failed (HTTP %d)", e.HTTPStatus)
}

type Client struct {
	endpoint   string
	token      string
	botID      string
	httpClient *http.Client
	nextEcho   atomic.Uint64

	mu       sync.Mutex
	running  bool
	checking bool
	active   *connection
	status   string

	// Fixed production defaults, adjustable only inside package-local tests.
	backoffMin time.Duration
	backoffMax time.Duration
}

// New performs configuration validation only. ws:// is supported for local
// NapCat installations; use wss:// across any untrusted network. Credentials
// in URLs, query strings, redirects, and API-only/event-only URLs are rejected.
func New(opts Options) (*Client, error) {
	u, err := url.Parse(opts.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("onebot: use a ws/wss URL without userinfo, query or fragment")
	}
	if path := strings.TrimRight(u.Path, "/"); path == "/api" || path == "/event" {
		return nil, errors.New("onebot: use the combined API/event WebSocket endpoint, usually /")
	}
	if !validAccountID(opts.BotID) {
		return nil, errors.New("onebot: an expected numeric BotID is required")
	}
	if len(opts.Token) > 16<<10 || strings.IndexFunc(opts.Token, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
		return nil, errors.New("onebot: invalid access token format")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 32 << 10
	return &Client{
		endpoint: u.String(), token: opts.Token, botID: opts.BotID,
		httpClient: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		status: "idle", backoffMin: time.Second, backoffMax: 30 * time.Second,
	}, nil
}

func (c *Client) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Client) setRunStatus(status string) {
	c.mu.Lock()
	c.status = status
	c.mu.Unlock()
}

// Run reconnects transient transport failures with capped exponential backoff
// and jitter, rechecking get_login_info before EVERY connection is published.
// Identity/protocol/API/auth failures fail closed instead of retrying forever.
// A single ordered handler runs independently of the socket reader, so a handler
// may call Send without blocking echo responses. Handlers must return promptly;
// cancellation closes the socket and pending RPCs even if a handler is slow.
func (c *Client) Run(ctx context.Context, handler func(Event)) error {
	if handler == nil {
		return errors.New("onebot: a message handler is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return ErrAlreadyRunning
	}
	c.running, c.status = true, "connecting"
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running, c.active = false, nil
		if ctx.Err() != nil {
			c.status = "stopped"
		}
		c.mu.Unlock()
	}()

	backoff := c.backoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		s, err := c.connect(ctx, true)
		if err == nil {
			c.mu.Lock()
			c.active, c.status = s, "connected"
			c.mu.Unlock()
			err = c.serve(ctx, s, handler)
			c.mu.Lock()
			if c.active == s {
				c.active = nil
			}
			c.mu.Unlock()
			s.stop(err)
			<-s.readerDone
			if time.Since(started) > time.Minute {
				backoff = c.backoffMin
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terminalError(err) {
			if errors.Is(err, ErrBotIDMismatch) {
				c.setRunStatus("identity_mismatch")
			} else {
				c.setRunStatus("connection_error")
			}
			return err
		}
		c.setRunStatus("reconnecting")
		// Jitter stays below the cap; no reconnect ever retries a business send.
		delay := backoff/2 + time.Duration(rand.Int64N(max(1, int64(backoff/2))))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, c.backoffMax)
	}
}

func (c *Client) serve(ctx context.Context, s *connection, handler func(Event)) error {
	defer s.stop(ErrDisconnected)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return s.failure()
		case e := <-s.events:
			if err := ctx.Err(); err != nil {
				return err
			}
			select {
			case <-s.done:
				return s.failure()
			default:
			}
			handler(e)
		}
	}
}

// Check creates an isolated short-lived connection and calls get_login_info.
// It does not publish the connection to Send or deliver any events to handlers.
func (c *Client) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.checking {
		c.mu.Unlock()
		return ErrCheckInProgress
	}
	c.checking = true
	if !c.running {
		c.status = "checking"
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.checking = false
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	s, err := c.connect(ctx, false)
	if s != nil {
		s.stop(ErrDisconnected)
		<-s.readerDone
	}
	c.mu.Lock()
	if !c.running {
		switch {
		case err == nil:
			c.status = "check_ok"
		case errors.Is(err, ErrBotIDMismatch):
			c.status = "identity_mismatch"
		default:
			c.status = "check_error"
		}
	}
	c.mu.Unlock()
	return err
}

// Send sends a single text segment over an already verified connection. It
// never connects, retries, or interprets CQ codes. Account IDs are held as
// strings and encoded as exact JSON numbers, without passing through float64.
func (c *Client) Send(ctx context.Context, e Event, text string) error {
	if e.BotID != "" && e.BotID != c.botID {
		return ErrBotIDMismatch
	}
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || len(text) > MaxTextBytes {
		return errors.New("onebot: bounded nonempty UTF-8 text is required")
	}
	action, target, field := "send_private_msg", e.UserID, "user_id"
	if e.IsGroup {
		action, target, field = "send_group_msg", e.GroupID, "group_id"
	}
	if !validAccountID(target) || !e.IsGroup && target == c.botID {
		return errors.New("onebot: invalid message target")
	}
	c.mu.Lock()
	s := c.active
	c.mu.Unlock()
	if s == nil || !s.verified.Load() {
		return ErrNotConnected
	}
	params := map[string]any{
		field:         json.Number(target),
		"message":     []textSegment{{Type: "text", Data: textData{Text: text}}},
		"auto_escape": true,
	}
	data, err := c.call(ctx, s, action, params)
	if err != nil {
		return err
	}
	var response struct {
		MessageID wireID `json:"message_id"`
	}
	if json.Unmarshal(data, &response) != nil || !validMessageID(string(response.MessageID)) {
		return errors.New("onebot: invalid send response; delivery state unknown, do not blindly retry")
	}
	return nil
}

func (c *Client) connect(ctx context.Context, receiveEvents bool) (*connection, error) {
	header := make(http.Header)
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	dialCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	ws, res, err := websocket.Dial(dialCtx, c.endpoint, &websocket.DialOptions{
		HTTPClient: c.httpClient, HTTPHeader: header, CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if res != nil {
			return nil, &HandshakeError{HTTPStatus: res.StatusCode}
		}
		return nil, errors.New("onebot: WebSocket connection failed")
	}
	ws.SetReadLimit(MaxBodyBytes)
	readCtx, readCancel := context.WithCancel(ctx)
	s := &connection{
		ws: ws, ctx: readCtx, cancel: readCancel, expectedBot: c.botID,
		receiveEvents: receiveEvents, pending: make(map[string]pendingRequest),
		done: make(chan struct{}), readerDone: make(chan struct{}), events: make(chan Event, maxQueuedEvents),
	}
	go s.readLoop()
	_, err = c.call(ctx, s, "get_login_info", struct{}{})
	if err != nil {
		s.stop(err)
		<-s.readerDone
		return nil, err
	}
	if ctx.Err() != nil {
		s.stop(ctx.Err())
		<-s.readerDone
		return nil, ctx.Err()
	}
	select {
	case <-s.done:
		<-s.readerDone
		return nil, s.failure()
	default:
	}
	return s, nil
}

func (c *Client) call(ctx context.Context, s *connection, action string, params any) (json.RawMessage, error) {
	echo := "opsagent-" + strconv.FormatUint(c.nextEcho.Add(1), 10)
	return s.call(ctx, echo, action, params)
}

func terminalError(err error) bool {
	if errors.Is(err, ErrBotIDMismatch) || errors.Is(err, ErrProtocol) || errors.Is(err, ErrBackpressure) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return true
	}
	var handshake *HandshakeError
	return errors.As(err, &handshake) && handshake.HTTPStatus >= 200 && handshake.HTTPStatus < 500
}

type rpcResult struct {
	data json.RawMessage
	err  error
}

type pendingRequest struct {
	waiter chan rpcResult
	login  bool
}

type connection struct {
	ws            *websocket.Conn
	ctx           context.Context
	cancel        context.CancelFunc
	expectedBot   string
	receiveEvents bool
	verified      atomic.Bool
	events        chan Event
	done          chan struct{}
	readerDone    chan struct{}
	once          sync.Once

	mu      sync.Mutex
	pending map[string]pendingRequest
	err     error
}

func (s *connection) stop(err error) {
	if err == nil {
		err = ErrDisconnected
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.pending = nil
		s.verified.Store(false)
		close(s.done)
		s.mu.Unlock()
		s.cancel()
		_ = s.ws.CloseNow()
	})
}

func (s *connection) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return ErrDisconnected
	}
	return s.err
}

func (s *connection) call(ctx context.Context, echo, action string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Action string `json:"action"`
		Params any    `json:"params"`
		Echo   string `json:"echo"`
	}{action, params, echo})
	if err != nil || len(body) > MaxBodyBytes {
		return nil, errors.New("onebot: request cannot be encoded within size limit")
	}
	waiter := make(chan rpcResult, 1)
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, err
	}
	if len(s.pending) >= maxPending {
		s.mu.Unlock()
		return nil, ErrTooManyPending
	}
	s.pending[echo] = pendingRequest{waiter: waiter, login: action == "get_login_info"}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, echo)
		s.mu.Unlock()
	}()
	// coder/websocket serializes concurrent writes and honors this deadline.
	if err := s.ws.Write(ctx, websocket.MessageText, body); err != nil {
		s.stop(ErrDisconnected)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrDisconnected
	}
	select {
	case <-ctx.Done():
		// No retry: the server may have sent the message but lost the reply.
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.failure()
	case result := <-waiter:
		return result.data, result.err
	}
}

func (s *connection) readLoop() {
	defer close(s.readerDone)
	defer s.stop(ErrDisconnected)
	for {
		kind, body, err := s.ws.Read(s.ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText || !utf8.Valid(body) || len(body) > MaxBodyBytes {
			s.stop(ErrProtocol)
			return
		}
		var envelope struct {
			PostType string          `json:"post_type"`
			Echo     json.RawMessage `json:"echo"`
			Status   string          `json:"status"`
			RetCode  *int64          `json:"retcode"`
			Data     json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			s.stop(ErrProtocol)
			return
		}
		if envelope.PostType != "" {
			// Never trust or buffer any event before get_login_info succeeded.
			if !s.verified.Load() || !s.receiveEvents {
				continue
			}
			e, ok, err := parseEvent(body, s.expectedBot)
			if err != nil {
				s.stop(err)
				return
			}
			if ok {
				select {
				case s.events <- e:
				case <-s.ctx.Done():
					return
				default:
					// Never let a blocked handler stall API replies indefinitely.
					s.stop(ErrBackpressure)
					return
				}
			}
			continue
		}
		var echo string
		if json.Unmarshal(envelope.Echo, &echo) != nil || echo == "" || len(echo) > 128 {
			// Uncorrelated frames cannot complete a pending API request.
			continue
		}
		s.mu.Lock()
		pending, found := s.pending[echo]
		delete(s.pending, echo)
		s.mu.Unlock()
		if !found {
			continue
		}
		var result rpcResult
		if envelope.RetCode == nil {
			result.err = ErrProtocol
		} else if envelope.Status != "ok" || *envelope.RetCode != 0 {
			result.err = &APIError{RetCode: *envelope.RetCode}
		} else {
			result.data = envelope.Data
		}
		if pending.login && result.err == nil {
			var login struct {
				UserID wireID `json:"user_id"`
			}
			if json.Unmarshal(result.data, &login) != nil || !validAccountID(string(login.UserID)) {
				result.err = ErrProtocol
			} else if string(login.UserID) != s.expectedBot {
				result.err = ErrBotIDMismatch
			} else {
				// Commit verification IN the reader before reading the next
				// frame. Otherwise events immediately after the login response
				// could be dropped while the dialing goroutine is unscheduled.
				s.mu.Lock()
				if s.err == nil {
					s.verified.Store(true)
				}
				s.mu.Unlock()
			}
		}
		pending.waiter <- result
		if pending.login && result.err != nil {
			s.stop(result.err)
			return
		}
	}
}
