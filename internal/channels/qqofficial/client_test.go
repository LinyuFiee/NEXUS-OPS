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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.AppID == "" {
		opts.AppID = "11111111"
	}
	if opts.Secret == "" {
		opts.Secret = "local-test-secret"
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOfficialSignatureVectors(t *testing.T) {
	// Public fixture copied verbatim from Tencent's official botgo test.
	// https://github.com/tencent-connect/botgo/blob/fe31c0dfe469001e0f783d2f07e7de7bd08b403f/interaction/signature/interaction_test.go
	tests := []struct{ secret, body, timestamp, signature string }{
		{
			"123456abcdef",
			`{"id":"ROBOT1.0_veoihSEXDc8Q.g-6eLpNIa11bH8MisOjn-m-LKxCPntMk6exUXgcWCGpVO7L2QKTNZzjZzFFDSbiOFcqAPWyVA!!","content":"哦一下","timestamp":"2024-10-15T16:33:15+08:00","author":{"id":"675860273","user_openid":"675860273"}}`,
			"1728981195",
			"e949b5b94ef4103df903fb031d1d16e358db3db83e79e117edd404c8508be3ce8a76d7bad1bed353194c126a1a5915b4ad8b5288c1191cc53a12acffccd82004",
		},
	}
	for i, tc := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			c := newTestClient(t, Options{Secret: tc.secret})
			seconds, _ := strconv.ParseInt(tc.timestamp, 10, 64)
			if err := c.Verify([]byte(tc.body), tc.signature, tc.timestamp, time.Unix(seconds, 0)); err != nil {
				t.Fatalf("official vector: %v", err)
			}
			if err := c.Verify([]byte(tc.body+" "), tc.signature, tc.timestamp, time.Unix(seconds, 0)); !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("changed raw body: %v", err)
			}
		})
	}
}

func TestSignatureWindowAndLimits(t *testing.T) {
	c := newTestClient(t, Options{})
	body := []byte(`{"op":0,"d":{}}`)
	now := time.Unix(1800000000, 0)
	for _, shift := range []time.Duration{-MaxTimestampSkew, 0, MaxTimestampSkew} {
		ts := strconv.FormatInt(now.Add(shift).Unix(), 10)
		sig := hex.EncodeToString(ed25519.Sign(c.privateKey, append([]byte(ts), body...)))
		if err := c.Verify(body, sig, ts, now); err != nil {
			t.Fatalf("boundary %v: %v", shift, err)
		}
	}
	for _, shift := range []time.Duration{-MaxTimestampSkew - time.Second, MaxTimestampSkew + time.Second} {
		ts := strconv.FormatInt(now.Add(shift).Unix(), 10)
		sig := hex.EncodeToString(ed25519.Sign(c.privateKey, append([]byte(ts), body...)))
		if err := c.Verify(body, sig, ts, now); !errors.Is(err, ErrTimestamp) {
			t.Fatalf("outside window: %v", err)
		}
	}
	for _, ts := range []string{"", "-1", "+1800000000", "1800000000.0", "1e9", " 1800000000", "9223372036854775808"} {
		if err := c.Verify(body, strings.Repeat("00", 64), ts, now); !errors.Is(err, ErrTimestamp) {
			t.Fatalf("invalid timestamp %q: %v", ts, err)
		}
	}
	for _, sig := range []string{"", "zz", strings.Repeat("00", 64), strings.Repeat("ff", 64)} {
		if err := c.Verify(body, sig, "1800000000", now); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("invalid signature: %v", err)
		}
	}
	if err := c.Verify(make([]byte, MaxBodyBytes+1), "", "", now); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatal(err)
	}
}

func TestOfficialChallengeVector(t *testing.T) {
	// https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/event-emit/webhook.html
	c := newTestClient(t, Options{Secret: "DG5g3B4j9X2KOErG"})
	body := []byte(`{"d":{"plain_token":"Arq0D5A61EgUu4OxUvOp","event_ts":"1725442341"},"op":13}`)
	out, handled, err := c.Challenge(body)
	if err != nil || !handled {
		t.Fatalf("challenge: %v, %v", handled, err)
	}
	want := `{"plain_token":"Arq0D5A61EgUu4OxUvOp","signature":"87befc99c42c651b3aac0278e71ada338433ae26fcb24307bdc5ad38c1adc2d01bcfcadc0842edac85e85205028a1132afe09280305f13aa6909ffc2d652c706"}`
	if string(out) != want {
		t.Fatalf("challenge differs from official fixture: %s", out)
	}
	if _, handled, err = c.Challenge([]byte(`{"op":0,"d":{}}`)); err != nil || handled {
		t.Fatalf("nonchallenge: %v, %v", handled, err)
	}
	for _, body := range []string{`{"op":13,"d":{}}`, `{"op":13,"d":{"plain_token":"x","event_ts":123}}`, `{"op":13,"d":{"plain_token":"x","event_ts":"-1"}}`} {
		if _, handled, err = c.Challenge([]byte(body)); err == nil || !handled {
			t.Fatalf("bad challenge: handled=%v error=%v", handled, err)
		}
	}
}

func TestParseTextEvents(t *testing.T) {
	c := newTestClient(t, Options{})
	tests := []struct {
		name, body string
		want       Event
	}{
		{"c2c", `{"op":0,"id":"event-1","t":"C2C_MESSAGE_CREATE","d":{"id":"ROBOT1.0_msg!!","author":{"id":"legacy","user_openid":"USER_A"},"content":" /status ","message_type":0}}`, Event{ID: "event-1", BotID: "11111111", UserID: "USER_A", Text: "/status", MessageID: "ROBOT1.0_msg!!"}},
		{"group", `{"op":0,"id":"event-2","t":"GROUP_AT_MESSAGE_CREATE","d":{"id":"msg-2","group_openid":"GROUP_A","group_id":"legacy-group","author":{"id":"legacy","member_openid":"MEMBER_A"},"content":" /status "}}`, Event{ID: "event-2", BotID: "11111111", UserID: "MEMBER_A", GroupID: "GROUP_A", Text: "/status", MessageID: "msg-2", IsGroup: true, Mentioned: true}},
		{"legacy-sdk", `{"op":0,"t":"GROUP_AT_MESSAGE_CREATE","d":{"id":"msg-3","group_id":"GROUP_A","author":{"id":"MEMBER_A"},"content":"literal &amp; [CQ:xml,data=x]"}}`, Event{ID: "msg-3", BotID: "11111111", UserID: "MEMBER_A", GroupID: "GROUP_A", Text: "literal &amp; [CQ:xml,data=x]", MessageID: "msg-3", IsGroup: true, Mentioned: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := c.Parse([]byte(tc.body))
			if err != nil || len(events) != 1 || events[0] != tc.want {
				t.Fatalf("got %#v, %v, want %#v", events, err, tc.want)
			}
		})
	}
}

func TestParseIgnoresUnsupportedAndRejectsMalformed(t *testing.T) {
	c := newTestClient(t, Options{})
	for _, body := range []string{
		`{"op":0,"t":"MESSAGE_CREATE","d":{}}`,
		`{"op":13,"d":{}}`,
		`{"op":0,"t":"C2C_MESSAGE_CREATE","d":{"id":"m","author":{"user_openid":"u","bot":true},"content":"/status"}}`,
		`{"op":0,"t":"C2C_MESSAGE_CREATE","d":{"id":"m","author":{"user_openid":"u"},"content":"/status","message_type":3}}`,
		`{"op":0,"t":"C2C_MESSAGE_CREATE","d":{"id":"m","author":{"user_openid":"u"},"content":"/status","attachments":[{"url":"file:///do-not-read"}]}}`,
		`{"op":0,"t":"C2C_MESSAGE_CREATE","d":{"id":"m","author":{"user_openid":"u"},"content":"/status","msg_elements":[{"content":"do-not-run"}]}}`,
	} {
		if events, err := c.Parse([]byte(body)); err != nil || len(events) != 0 {
			t.Fatalf("unsupported: %#v %v", events, err)
		}
	}
	for _, body := range []string{
		`{}`, `null`, `{"op":0} {}`, `{"op":"0"}`,
		`{"op":0,"t":"C2C_MESSAGE_CREATE","d":{"id":"m","author":{"user_openid":9007199254740993},"content":"/status"}}`,
		`{"op":0,"t":"GROUP_AT_MESSAGE_CREATE","d":{"id":"m","author":{"member_openid":"u"},"content":"/status"}}`,
		`{"op":0,"t":"C2C_MESSAGE_CREATE","d":{"author":{"user_openid":"u"},"content":"/status"}}`,
	} {
		if _, err := c.Parse([]byte(body)); err == nil {
			t.Fatalf("accepted invalid payload %s", body)
		}
	}
	if _, err := c.Parse(make([]byte, MaxBodyBytes+1)); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatal(err)
	}
}

func TestNewOfflineAndURLValidation(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	c := newTestClient(t, Options{APIBase: server.URL, TokenURL: server.URL + "/token"})
	if c.Status() != "idle" || calls.Load() != 0 {
		t.Fatal("New made a network call")
	}
	for _, endpoint := range []string{"http://remote.example", "https://user:pass@example.test", "https://example.test/?token=secret", "https://example.test/#fragment", "//example.test"} {
		if _, err := New(Options{AppID: "123", Secret: "test", APIBase: endpoint}); err == nil {
			t.Fatalf("accepted unsafe endpoint %s", endpoint)
		}
	}
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing credentials accepted")
	}
}

func TestCheckTokenBodyCacheAndExpiryForms(t *testing.T) {
	for _, expires := range []string{`"7200"`, `7200`} {
		t.Run(expires, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/token" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected token request")
				}
				var req struct{ AppID, ClientSecret string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AppID != "11111111" || req.ClientSecret != "local-test-secret" {
					t.Errorf("bad token credentials payload: %v", err)
				}
				fmt.Fprintf(w, `{"access_token":"mock-token","expires_in":%s}`, expires)
			}))
			defer server.Close()
			c := newTestClient(t, Options{TokenURL: server.URL + "/token"})
			for i := 0; i < 2; i++ {
				if err := c.Check(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 1 || c.Status() != "token_valid" {
				t.Fatalf("cache/status: %d, %s", calls.Load(), c.Status())
			}
		})
	}
}

func TestTokenErrorsNoRetryOrSecrets(t *testing.T) {
	for _, response := range []string{
		`{"code":100001,"message":"local-test-secret mock-token"}`,
		`{"access_token":"mock-token","expires_in":"0"}`,
		`{"access_token":"mock-token","expires_in":"9223372036854775807"}`,
		`{"access_token":"mock-token","expires_in":7.2}`,
		`{"access_token":"injected\r\nAuthorization: other","expires_in":7200}`,
	} {
		t.Run(response, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "120")
				io.WriteString(w, response)
			}))
			defer server.Close()
			c := newTestClient(t, Options{TokenURL: server.URL})
			for i := 0; i < 2; i++ {
				err := c.Check(context.Background())
				if err == nil || strings.Contains(err.Error(), "local-test-secret") || strings.Contains(err.Error(), "mock-token") {
					t.Fatalf("unsafe token error: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("failed token fetch should be coalesced/cooldown, not retried")
			}
		})
	}
}

func TestNoCredentialRedirectAndBoundedResponses(t *testing.T) {
	var leaked atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	for _, status := range []int{301, 302, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, destination.URL, status)
			}))
			defer server.Close()
			c := newTestClient(t, Options{TokenURL: server.URL, HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { t.Error("caller redirect policy was used"); return nil }}})
			if err := c.Check(context.Background()); err == nil {
				t.Fatal("redirect accepted")
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("credential-bearing redirect followed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, io.LimitReader(zeroReader{}, MaxResponseBytes+1))
	}))
	defer server.Close()
	if err := newTestClient(t, Options{TokenURL: server.URL}).Check(context.Background()); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("response limit: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func TestSendTextAndOne401Refresh(t *testing.T) {
	var tokens, sends atomic.Int64
	var sentBodies [][]byte
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			n := tokens.Add(1)
			fmt.Fprintf(w, `{"access_token":"token-%d","expires_in":"7200"}`, n)
			return
		}
		n := sends.Add(1)
		if r.URL.Path != "/v2/groups/GROUP_A/messages" || r.Header.Get("X-Union-Appid") != "11111111" || r.Method != "POST" {
			t.Errorf("unexpected message endpoint/header: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sentBodies = append(sentBodies, body)
		mu.Unlock()
		if n == 1 {
			if r.Header.Get("Authorization") != "QQBot token-1" {
				t.Error("missing initial bearer")
			}
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":11244}`)
			return
		}
		if r.Header.Get("Authorization") != "QQBot token-2" {
			t.Error("missing refreshed bearer")
		}
		io.WriteString(w, `{"id":"reply-1"}`)
	}))
	defer server.Close()
	c := newTestClient(t, Options{APIBase: server.URL, TokenURL: server.URL + "/token"})
	e := Event{BotID: "11111111", GroupID: "GROUP_A", MessageID: "original-1", IsGroup: true, Mentioned: true}
	if err := c.Send(context.Background(), e, "literal [CQ:image,file=file:///no] & <script>"); err != nil {
		t.Fatal(err)
	}
	if tokens.Load() != 2 || sends.Load() != 2 || len(sentBodies) != 2 || !bytes.Equal(sentBodies[0], sentBodies[1]) {
		t.Fatalf("unexpected refresh/retry counts %d %d", tokens.Load(), sends.Load())
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(sentBodies[0], &body) != nil || string(body["msg_type"]) != "0" || string(body["msg_seq"]) != "1" || string(body["msg_id"]) != `"original-1"` || len(body) != 4 {
		t.Fatalf("not a passive text-only request: %s", sentBodies[0])
	}
}

func TestSendDoesNotBlindlyRetry(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var tokens, sends atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens.Add(1)
					io.WriteString(w, `{"access_token":"token","expires_in":7200}`)
					return
				}
				sends.Add(1)
				if r.URL.Path != "/v2/users/USER_A/messages" {
					t.Error("bad C2C target")
				}
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				io.WriteString(w, `{"code":40034100,"message":"local-test-secret token"}`)
			}))
			defer server.Close()
			c := newTestClient(t, Options{APIBase: server.URL, TokenURL: server.URL + "/token"})
			err := c.Send(context.Background(), Event{UserID: "USER_A", MessageID: "msg"}, "test")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.RetryAfter != time.Minute || strings.Contains(err.Error(), "secret") {
				t.Fatalf("API error: %v", err)
			}
			want := int64(1)
			if status == 401 {
				want = 2
			}
			if sends.Load() != want || tokens.Load() != want {
				t.Fatalf("unbounded/repeated requests: token=%d send=%d", tokens.Load(), sends.Load())
			}
		})
	}
}

func TestConcurrentTokenRefreshEvenWhenTokenStringUnchanged(t *testing.T) {
	const workers = 12
	var tokens atomic.Int64
	var initial sync.WaitGroup
	initial.Add(workers)
	var attempts sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens.Add(1)
			io.WriteString(w, `{"access_token":"same-token","expires_in":7200}`)
			return
		}
		var req struct {
			MsgID string `json:"msg_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if _, loaded := attempts.LoadOrStore(req.MsgID, true); !loaded {
			initial.Done()
			initial.Wait()
			w.WriteHeader(401)
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `{"id":"reply"}`)
	}))
	defer server.Close()
	c := newTestClient(t, Options{APIBase: server.URL, TokenURL: server.URL + "/token"})
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Send(ctx, Event{UserID: "USER_A", MessageID: fmt.Sprintf("message-%d", i)}, "text"); err != nil {
				t.Errorf("concurrent send: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if tokens.Load() != 2 {
		t.Fatalf("401 stampede fetched %d tokens, want exactly 2", tokens.Load())
	}
}

func TestCancelledRefreshWaiterAndInvalidSendAreOffline(t *testing.T) {
	c := newTestClient(t, Options{})
	for _, e := range []Event{{UserID: "USER_A"}, {UserID: "../escape", MessageID: "m"}, {UserID: "USER_A", MessageID: "m", BotID: "wrong"}} {
		if err := c.Send(context.Background(), e, "text"); err == nil {
			t.Fatal("invalid send accepted")
		}
	}
	c.refreshDone = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	close(c.refreshDone)
}
