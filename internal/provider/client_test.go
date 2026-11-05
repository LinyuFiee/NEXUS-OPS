package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamingToolArgumentsAreAssembled(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing credentials")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.log\"}"}}]}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer s.Close()
	m, e := New(s.URL, "mock", "test-key").Complete(context.Background(), []Message{{Role: "user", Content: "read"}}, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Arguments != `{"path":"a.log"}` {
		t.Fatalf("bad tool assembly: %+v", m)
	}
}
func TestIncompleteStreamRejected(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`)
	}))
	defer s.Close()
	_, e := New(s.URL, "mock", "test-key").Complete(context.Background(), nil, nil, nil)
	if e == nil {
		t.Fatal("incomplete stream accepted")
	}
}
func TestRedirectDoesNotLeakToken(t *testing.T) {
	received := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received = true }))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 307) }))
	defer s.Close()
	_, e := New(s.URL, "mock", "test-key").Complete(context.Background(), nil, nil, nil)
	if e == nil || received {
		t.Fatal("redirect followed")
	}
}
func TestProviderErrorDoesNotEchoBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); fmt.Fprint(w, "secret-value") }))
	defer s.Close()
	_, e := New(s.URL, "mock", "test-key").Complete(context.Background(), nil, nil, nil)
	if e == nil || strings.Contains(e.Error(), "secret-value") {
		t.Fatal("error body exposed")
	}
}
