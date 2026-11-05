package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"opsagent/internal/agent"
	"opsagent/internal/config"
	"opsagent/internal/memory"
	"opsagent/internal/setup"
	"opsagent/internal/store"
	"opsagent/internal/tools"
)

func TestSetupLoginAndProtectedAPI(t *testing.T) {
	db, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	work := t.TempDir()
	cfg, e := config.Load(db, work)
	if e != nil {
		t.Fatal(e)
	}
	mem, e := memory.New(db.DB)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	engine := agent.New(ctx, db, cfg, mem, tools.New(cfg.Get, []string{db.Dir}))
	defer func() { cancel(); engine.Wait() }()
	srv, e := New(db, cfg, mem, engine, nil, Options{Origin: "http://127.0.0.1", AllowLoopbackDynamic: true})
	if e != nil {
		t.Fatal(e)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	request := func(method, path string, p any) (int, []byte) {
		t.Helper()
		var body io.Reader
		if p != nil {
			b, _ := json.Marshal(p)
			body = bytes.NewReader(b)
		}
		r, _ := http.NewRequest(method, ts.URL+path, body)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Ops-Request", "1")
		res, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	if code, _ := request("GET", "/api/tasks", nil); code != 401 {
		t.Fatalf("unauthenticated task API: %d", code)
	}
	setupCode, e := setup.IssueCode(db)
	if e != nil {
		t.Fatal(e)
	}
	if code, b := request("POST", "/api/setup/unlock", map[string]string{"code": setupCode}); code != 200 {
		t.Fatalf("unlock %d %s", code, b)
	}
	if code, _ := request("POST", "/api/setup/unlock", map[string]string{"code": setupCode}); code != 403 {
		t.Fatalf("setup replay %d", code)
	}
	code, b := request("POST", "/api/setup/complete", map[string]any{"config": cfg.Get(), "secrets": config.Secrets{}})
	if code != 200 {
		t.Fatalf("setup complete %d %s", code, b)
	}
	var result struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(b, &result)
	if code, _ := request("GET", "/api/setup/config", nil); code != 401 {
		t.Fatal("setup remained open")
	}
	if code, _ := request("POST", "/api/login", map[string]string{"token": result.Token}); code != 200 {
		t.Fatalf("login %d", code)
	}
	if code, _ := request("GET", "/api/tasks", nil); code != 200 {
		t.Fatalf("authenticated task API %d", code)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/tasks", bytes.NewBufferString(`{"message":"hello"}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ops-Request", "1")
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	if code, b := request("POST", "/api/tasks", map[string]string{"message": "inspect"}); code != 400 {
		t.Fatalf("missing model not rejected %d %s", code, b)
	}
	if code, _ := request("POST", "/api/logout", map[string]bool{}); code != 200 {
		t.Fatal("logout failed")
	}
	if code, _ := request("GET", "/api/tasks", nil); code != 401 {
		t.Fatal("logout session still valid")
	}
}
