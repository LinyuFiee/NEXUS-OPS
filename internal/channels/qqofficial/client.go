// Package qqofficial implements QQ's signed HTTP callback and passive text replies.
// New is offline. The caller owns enable/dry-run policy, HTTP body limits before
// reading, and event deduplication; Parse and Challenge require prior Verify.
package qqofficial

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Defaults follow the current official access-token and API-call documents,
	// not the older bots.qq.com/api.sgroup.qq.com defaults still in botgo.
	DefaultAPIBase  = "https://api.bot.qq.com"
	DefaultTokenURL = "https://api.bot.qq.com/app/getAppAccessToken"
	HeaderSignature = "X-Signature-Ed25519"
	HeaderTimestamp = "X-Signature-Timestamp"

	MaxBodyBytes     = 1 << 20
	MaxResponseBytes = 1 << 20
	MaxTextBytes     = 16 << 10
	MaxTimestampSkew = 5 * time.Minute
	requestTimeout   = 10 * time.Second
)

var (
	ErrInvalidSignature = errors.New("qqofficial: invalid callback signature")
	ErrTimestamp        = errors.New("qqofficial: callback timestamp outside allowed window or invalid")
	ErrBodyTooLarge     = errors.New("qqofficial: body exceeds size limit")
	ErrInvalidPayload   = errors.New("qqofficial: invalid callback payload")
)

type Options struct {
	AppID      string
	Secret     string
	APIBase    string
	TokenURL   string
	HTTPClient *http.Client
}

type Event struct {
	ID        string
	BotID     string // The configured AppID, not a QQ account number.
	UserID    string // user_openid (C2C) or member_openid (group).
	GroupID   string // group_openid, not the group's numeric QQ number.
	Text      string
	MessageID string // d.id, used for passive replies.
	IsGroup   bool
	Mentioned bool // True only for the GROUP_AT_MESSAGE_CREATE event type.
}

// APIError deliberately excludes remote response text, URLs and credentials.
// No rate-limit or transport failure is automatically retried.
type APIError struct {
	HTTPStatus int
	Code       int64
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("qqofficial: API rejected request (HTTP %d, code %d)", e.HTTPStatus, e.Code)
}

func (e *APIError) RateLimited() bool {
	return e.HTTPStatus == http.StatusTooManyRequests || e.Code == 100001 || e.Code == 40034100
}

type cachedToken struct {
	value      string
	validUntil time.Time
	generation uint64
}

type Client struct {
	appID      string
	secret     string
	apiBase    string
	tokenURL   string
	httpClient *http.Client
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey

	tokenMu     sync.Mutex
	token       cachedToken
	refreshDone chan struct{}
	refreshErr  error
	retryAt     time.Time

	statusMu sync.RWMutex
	status   string
}

// New validates configuration and derives a signing key without network I/O.
func New(opts Options) (*Client, error) {
	if !validOpenID(opts.AppID) || len(opts.Secret) == 0 || len(opts.Secret) > 4096 {
		return nil, errors.New("qqofficial: AppID and Secret are required and must have valid lengths")
	}
	if opts.APIBase == "" {
		opts.APIBase = DefaultAPIBase
	}
	if opts.TokenURL == "" {
		opts.TokenURL = DefaultTokenURL
	}
	base, err := validateURL(opts.APIBase)
	if err != nil {
		return nil, errors.New("qqofficial: invalid APIBase; use HTTPS without credentials, query or fragment")
	}
	tokenURL, err := validateURL(opts.TokenURL)
	if err != nil {
		return nil, errors.New("qqofficial: invalid TokenURL; use HTTPS without credentials, query or fragment")
	}

	hc := &http.Client{}
	if opts.HTTPClient != nil {
		*hc = *opts.HTTPClient
	}
	if hc.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxResponseHeaderBytes = 32 << 10
		hc.Transport = transport
	}
	if hc.Timeout <= 0 || hc.Timeout > requestTimeout {
		hc.Timeout = requestTimeout
	}
	// Even a same-origin 307/308 can forward the app secret or bearer token to
	// a different endpoint. Never follow redirects, including for test clients.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Official algorithm: repeat Secret, truncate to the first 32 bytes, then
	// derive Ed25519. NewKeyFromSeed is equivalent to GenerateKey over that seed.
	seed := opts.Secret
	for len(seed) < ed25519.SeedSize {
		seed += seed
	}
	privateKey := ed25519.NewKeyFromSeed([]byte(seed[:ed25519.SeedSize]))
	return &Client{
		appID: opts.AppID, secret: opts.Secret,
		apiBase: strings.TrimRight(base, "/"), tokenURL: tokenURL,
		httpClient: hc, privateKey: privateKey,
		publicKey: privateKey.Public().(ed25519.PublicKey), status: "idle",
	}, nil
}

func validateURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("invalid URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !(strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()) {
			return "", errors.New("HTTPS required")
		}
	}
	return u.String(), nil
}

func (c *Client) Status() string {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

func (c *Client) setStatus(status string) {
	c.statusMu.Lock()
	c.status = status
	c.statusMu.Unlock()
}

// Verify authenticates the exact raw body (do not re-encode JSON first).
// The five-minute clock window limits replay age; duplicate valid deliveries
// within that window are intentionally left to the durable router deduplicator.
// Keep the Linux host's clock synchronized. This local window is a hardening
// policy, not an assertion that the platform never retries older deliveries.
func (c *Client) Verify(body []byte, signature, timestamp string, now time.Time) error {
	if len(body) > MaxBodyBytes {
		return ErrBodyTooLarge
	}
	seconds, err := decimalInt(timestamp)
	if err != nil || seconds <= 0 {
		return ErrTimestamp
	}
	t := time.Unix(seconds, 0)
	if t.Before(now.Add(-MaxTimestampSkew)) || t.After(now.Add(MaxTimestampSkew)) {
		return ErrTimestamp
	}
	if len(signature) != hex.EncodedLen(ed25519.SignatureSize) {
		return ErrInvalidSignature
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || sig[63]&224 != 0 {
		return ErrInvalidSignature
	}
	content := make([]byte, 0, len(timestamp)+len(body))
	content = append(content, timestamp...)
	content = append(content, body...)
	if !ed25519.Verify(c.publicKey, content, sig) {
		return ErrInvalidSignature
	}
	c.setStatus("webhook_verified")
	return nil
}

type payload struct {
	Op   *int            `json:"op"`
	ID   string          `json:"id"`
	Type string          `json:"t"`
	Data json.RawMessage `json:"d"`
}

func decodePayload(body []byte) (payload, error) {
	var p payload
	if len(body) > MaxBodyBytes {
		return p, ErrBodyTooLarge
	}
	if !utf8.Valid(body) || json.Unmarshal(body, &p) != nil || p.Op == nil {
		return p, ErrInvalidPayload
	}
	return p, nil
}

// Challenge handles op 13 only AFTER Verify has succeeded. It signs
// event_ts+plain_token, not the JSON body and not the request header timestamp.
// An invalid op-13 body returns handled=true with an error.
func (c *Client) Challenge(body []byte) ([]byte, bool, error) {
	p, err := decodePayload(body)
	if err != nil {
		return nil, false, err
	}
	if *p.Op != 13 {
		return nil, false, nil
	}
	var d struct {
		PlainToken string `json:"plain_token"`
		EventTS    string `json:"event_ts"`
	}
	if json.Unmarshal(p.Data, &d) != nil || d.PlainToken == "" || len(d.PlainToken) > 4096 {
		return nil, true, ErrInvalidPayload
	}
	seconds, err := decimalInt(d.EventTS)
	if err != nil || seconds <= 0 {
		return nil, true, ErrInvalidPayload
	}
	signature := hex.EncodeToString(ed25519.Sign(c.privateKey, []byte(d.EventTS+d.PlainToken)))
	out, err := json.Marshal(struct {
		PlainToken string `json:"plain_token"`
		Signature  string `json:"signature"`
	}{d.PlainToken, signature})
	return out, true, err
}

// Parse handles only C2C_MESSAGE_CREATE and GROUP_AT_MESSAGE_CREATE dispatches.
// The caller MUST first Verify this same body. Unsupported events and non-text
// messages are ignored; quoted/card/media contents are never promoted to commands.
func (c *Client) Parse(body []byte) ([]Event, error) {
	p, err := decodePayload(body)
	if err != nil {
		return nil, err
	}
	if *p.Op != 0 || p.Type != "C2C_MESSAGE_CREATE" && p.Type != "GROUP_AT_MESSAGE_CREATE" {
		return nil, nil
	}
	var d struct {
		ID          string `json:"id"`
		Content     string `json:"content"`
		GroupOpenID string `json:"group_openid"`
		GroupID     string `json:"group_id"` // Older official botgo payloads.
		MessageType int    `json:"message_type"`
		Author      struct {
			ID           string `json:"id"`
			UserOpenID   string `json:"user_openid"`
			MemberOpenID string `json:"member_openid"`
			Bot          bool   `json:"bot"`
		} `json:"author"`
		Attachments []json.RawMessage `json:"attachments"`
		Elements    []json.RawMessage `json:"msg_elements"`
		Ark         json.RawMessage   `json:"ark_data"`
	}
	if json.Unmarshal(p.Data, &d) != nil {
		return nil, ErrInvalidPayload
	}
	if d.Author.Bot || d.MessageType != 0 || len(d.Attachments) > 0 || len(d.Elements) > 0 || len(d.Ark) > 0 && string(d.Ark) != "null" {
		return nil, nil
	}
	e := Event{ID: p.ID, BotID: c.appID, Text: strings.TrimSpace(d.Content), MessageID: d.ID, IsGroup: p.Type == "GROUP_AT_MESSAGE_CREATE"}
	if e.Text == "" {
		return nil, nil
	}
	if e.IsGroup {
		e.Mentioned = true
		e.UserID, e.GroupID = d.Author.MemberOpenID, d.GroupOpenID
		if e.GroupID == "" {
			e.GroupID = d.GroupID
		}
	} else {
		e.UserID = d.Author.UserOpenID
	}
	if e.UserID == "" {
		e.UserID = d.Author.ID
	}
	if e.UserID == c.appID {
		return nil, nil
	}
	if e.ID == "" {
		e.ID = e.MessageID
	}
	if !validOpenID(e.UserID) || e.IsGroup && !validOpenID(e.GroupID) || !validMessageID(e.MessageID) || !validMessageID(e.ID) || len(e.Text) > MaxTextBytes {
		return nil, ErrInvalidPayload
	}
	return []Event{e}, nil
}

// Check obtains (or verifies the local validity of) an access token only.
// It never sends a business message and does not prove webhook reachability.
func (c *Client) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_, err := c.accessToken(ctx, 0)
	if err != nil {
		c.setStatus("token_error")
		return err
	}
	c.setStatus("token_valid")
	return nil
}

// Send explicitly sends ONE passive text reply, with msg_seq=1. Active messages
// are not supported. The application must enforce disabled/dry-run policy before
// calling this method. Only an HTTP 401 permits one token refresh and one retry;
// the same msg_id/msg_seq protects that retry from creating a second reply.
func (c *Client) Send(ctx context.Context, e Event, text string) error {
	if e.BotID != "" && e.BotID != c.appID {
		return errors.New("qqofficial: event BotID does not match configured AppID")
	}
	if !validMessageID(e.MessageID) || strings.TrimSpace(text) == "" || len(text) > MaxTextBytes || !utf8.ValidString(text) {
		return errors.New("qqofficial: a message ID and bounded UTF-8 text are required for a passive reply")
	}
	kind, target := "users", e.UserID
	if e.IsGroup {
		kind, target = "groups", e.GroupID
	}
	if !validOpenID(target) {
		return errors.New("qqofficial: invalid reply target OpenID")
	}
	body, err := json.Marshal(struct {
		Content string `json:"content"`
		MsgType int    `json:"msg_type"`
		MsgID   string `json:"msg_id"`
		MsgSeq  int    `json:"msg_seq"`
	}{text, 0, e.MessageID, 1})
	if err != nil {
		return errors.New("qqofficial: cannot encode message")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*requestTimeout)
	defer cancel()
	token, err := c.accessToken(ctx, 0)
	if err != nil {
		c.setStatus("token_error")
		return err
	}
	endpoint := c.apiBase + "/v2/" + kind + "/" + url.PathEscape(target) + "/messages"
	for attempt := 0; attempt < 2; attempt++ {
		response, status, header, reqErr := c.post(ctx, endpoint, body, token.value)
		if reqErr != nil {
			c.setStatus("send_error")
			return reqErr
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			token, err = c.accessToken(ctx, token.generation)
			if err != nil {
				c.setStatus("token_error")
				return err
			}
			continue
		}
		if err = responseError(response, status, header); err != nil {
			c.setStatus("send_error")
			return err
		}
		var result struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(response, &result) != nil || !validMessageID(result.ID) {
			c.setStatus("send_error")
			return errors.New("qqofficial: invalid message response; delivery state unknown, do not blindly retry")
		}
		c.setStatus("ready")
		return nil
	}
	return errors.New("qqofficial: authentication retry exhausted")
}

// rejectedGeneration=0 means normal lookup. Generation, rather than token text,
// coalesces concurrent 401 refreshes even when QQ returns the SAME access token.
func (c *Client) accessToken(ctx context.Context, rejectedGeneration uint64) (cachedToken, error) {
	for {
		if err := ctx.Err(); err != nil {
			return cachedToken{}, err
		}
		c.tokenMu.Lock()
		if c.token.value != "" && time.Now().Before(c.token.validUntil) && (rejectedGeneration == 0 || c.token.generation != rejectedGeneration) {
			token := c.token
			c.tokenMu.Unlock()
			return token, nil
		}
		if done := c.refreshDone; done != nil {
			c.tokenMu.Unlock()
			select {
			case <-ctx.Done():
				return cachedToken{}, ctx.Err()
			case <-done:
				continue
			}
		}
		if c.refreshErr != nil && time.Now().Before(c.retryAt) {
			err := c.refreshErr
			c.tokenMu.Unlock()
			return cachedToken{}, err
		}
		done := make(chan struct{})
		c.refreshDone = done
		c.tokenMu.Unlock()

		token, err := c.fetchToken(ctx)
		c.tokenMu.Lock()
		if err == nil {
			token.generation = c.token.generation + 1
			c.token, c.refreshErr, c.retryAt = token, nil, time.Time{}
		} else {
			c.refreshErr = err
			cooldown := 3 * time.Second
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.RetryAfter > cooldown {
				cooldown = apiErr.RetryAfter
			}
			c.retryAt = time.Now().Add(cooldown)
		}
		c.refreshDone = nil
		close(done)
		c.tokenMu.Unlock()
		return token, err
	}
}

func (c *Client) fetchToken(ctx context.Context) (cachedToken, error) {
	body, _ := json.Marshal(struct {
		AppID        string `json:"appId"`
		ClientSecret string `json:"clientSecret"`
	}{c.appID, c.secret})
	response, status, header, err := c.post(ctx, c.tokenURL, body, "")
	if err != nil {
		return cachedToken{}, err
	}
	if err = responseError(response, status, header); err != nil {
		return cachedToken{}, err
	}
	var result struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if json.Unmarshal(response, &result) != nil || !validBearer(result.AccessToken) {
		return cachedToken{}, errors.New("qqofficial: invalid access-token response")
	}
	seconds, err := jsonDecimalInt(result.ExpiresIn)
	if err != nil || seconds <= 0 || seconds > 24*60*60 {
		return cachedToken{}, errors.New("qqofficial: invalid access-token expiration")
	}
	ttl := time.Duration(seconds) * time.Second
	margin := min(time.Minute, ttl/10)
	return cachedToken{value: result.AccessToken, validUntil: time.Now().Add(ttl - margin)}, nil
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte, accessToken string) ([]byte, int, http.Header, error) {
	if len(body) > MaxBodyBytes {
		return nil, 0, nil, ErrBodyTooLarge
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, errors.New("qqofficial: cannot construct request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opsagent/1")
	if accessToken != "" {
		req.Header.Set("Authorization", "QQBot "+accessToken)
		req.Header.Set("X-Union-Appid", c.appID)
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, nil, ctx.Err()
		}
		return nil, 0, nil, errors.New("qqofficial: HTTP request failed; delivery state may be unknown")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if len(data) > MaxResponseBytes {
		return nil, res.StatusCode, nil, ErrBodyTooLarge
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, res.StatusCode, nil, ctx.Err()
		}
		return nil, res.StatusCode, nil, errors.New("qqofficial: cannot read bounded response")
	}
	return data, res.StatusCode, res.Header, nil
}

func responseError(body []byte, status int, header http.Header) error {
	var result struct {
		Code    json.RawMessage `json:"code"`
		ErrCode json.RawMessage `json:"err_code"`
	}
	decodeErr := json.Unmarshal(body, &result)
	var code int64
	for _, raw := range []json.RawMessage{result.ErrCode, result.Code} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		n, err := jsonDecimalInt(raw)
		if err != nil {
			decodeErr = err
		} else if n != 0 && code == 0 {
			code = n
		}
	}
	if status < 200 || status >= 300 || code != 0 {
		return &APIError{HTTPStatus: status, Code: code, RetryAfter: retryAfter(header.Get("Retry-After"), time.Now())}
	}
	if decodeErr != nil {
		return errors.New("qqofficial: invalid JSON API response")
	}
	return nil
}

func retryAfter(s string, now time.Time) time.Duration {
	if seconds, err := decimalInt(s); err == nil {
		return time.Duration(min(seconds, int64(3600))) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil && t.After(now) {
		return min(t.Sub(now), time.Hour)
	}
	return 0
}

func decimalInt(s string) (int64, error) {
	if len(s) == 0 || len(s) > 19 {
		return 0, errors.New("invalid decimal integer")
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, errors.New("invalid decimal integer")
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

func jsonDecimalInt(raw []byte) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, errors.New("invalid JSON integer")
		}
		return decimalInt(s)
	}
	return decimalInt(string(raw))
}

func validOpenID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validMessageID(s string) bool {
	if len(s) == 0 || len(s) > 1024 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 127 {
			return false
		}
	}
	return true
}

func validBearer(s string) bool {
	if len(s) == 0 || len(s) > 16<<10 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r >= 127 {
			return false
		}
	}
	return true
}
