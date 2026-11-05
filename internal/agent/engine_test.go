package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"opsagent/internal/config"
	"opsagent/internal/memory"
	"opsagent/internal/provider"
	"opsagent/internal/store"
	"opsagent/internal/tools"
)

func TestToolLoopAndMemoryApproval(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []provider.Message `json:"messages"`
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
		}
		w.Header().Set("Content-Type", "application/json")
		done := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				done = true
			}
		}
		if done {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"已保存确认过的主机偏好。"}}]}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"请确认这条长期记忆。","tool_calls":[{"id":"call_mem","type":"function","function":{"name":"propose_memory","arguments":"{\"text\":\"测试主机用于 Nginx 日志分析\",\"category\":\"fact\"}"}}]}}]}`)
		}
	}))
	defer model.Close()
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
	c := config.Default(work)
	c.Model = config.Model{BaseURL: model.URL, Name: "mock"}
	if _, e = cfg.Initialize(c, config.Secrets{ModelKey: "local-test-key"}); e != nil {
		t.Fatal(e)
	}
	mem, e := memory.New(db.DB)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	engine := New(ctx, db, cfg, mem, tools.New(cfg.Get, []string{db.Dir}))
	defer func() { cancel(); engine.Wait() }()
	task, e := engine.Submit("", "请记住这台主机用于 Nginx 日志分析", false)
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(8 * time.Second)
	approved := false
	for time.Now().Before(deadline) {
		for _, a := range engine.Approvals() {
			if !approved {
				items, _ := mem.List(ctx, "owner", "", 10)
				if len(items) != 0 {
					t.Fatal("memory stored before approval")
				}
				if e = engine.Decide(a.ID, true); e != nil {
					t.Fatal(e)
				}
				if e = engine.Decide(a.ID, true); e == nil {
					t.Fatal("approval replay accepted")
				}
				approved = true
			}
		}
		task, _ = db.Task(task.ID)
		if task.Status == "completed" {
			break
		}
		if task.Status == "failed" {
			t.Fatalf("task failed: %s", task.Error)
		}
		time.Sleep(15 * time.Millisecond)
	}
	if task.Status != "completed" || !approved {
		t.Fatalf("loop incomplete: %+v", task)
	}
	items, _ := mem.List(ctx, "owner", "", 10)
	if len(items) != 1 {
		t.Fatalf("expected confirmed memory, got %d", len(items))
	}
}
func TestHistoryDoesNotReplayIncompleteTools(t *testing.T) {
	rows := []store.Message{}
	add := func(m provider.Message) { b, _ := json.Marshal(m); rows = append(rows, store.Message{Raw: b}) }
	add(provider.Message{Role: "user", Content: "safe"})
	add(provider.Message{Role: "assistant", Content: "done"})
	add(provider.Message{Role: "user", Content: "unfinished"})
	add(provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "x", Type: "function", Function: provider.Function{Name: "exec", Arguments: `{}`}}}})
	h := compactHistory(rows, 5000)
	if len(h) != 2 {
		t.Fatalf("incomplete tool sequence replayed: %+v", h)
	}
}
