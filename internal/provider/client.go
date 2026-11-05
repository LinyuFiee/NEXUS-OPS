package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
type Client struct {
	BaseURL, Model, Key string
	HTTP                *http.Client
}

func New(base, model, key string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), Model: model, Key: key, HTTP: &http.Client{Timeout: 100 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool, onText func(string)) (Message, error) {
	if c.Key == "" || c.Model == "" {
		return Message{}, errors.New("尚未配置模型密钥或名称，请先完成模型设置")
	}
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Hostname() == "" {
		return Message{}, errors.New("模型地址无效")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return Message{}, errors.New("远程模型必须使用 HTTPS")
	}
	body := map[string]any{"model": c.Model, "messages": messages, "stream": true, "max_tokens": 4096}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	b, e := json.Marshal(body)
	if e != nil {
		return Message{}, e
	}
	if len(b) > 1024*1024 {
		return Message{}, errors.New("模型请求过大，请缩短上下文")
	}
	req, e := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewReader(b))
	if e != nil {
		return Message{}, e
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, e := c.HTTP.Do(req)
	if e != nil {
		return Message{}, fmt.Errorf("模型连接失败: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Message{}, fmt.Errorf("模型服务返回 HTTP %d，请检查地址、凭据、额度或稍后重试", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var response struct {
			Choices []struct {
				Message Message `json:"message"`
			} `json:"choices"`
			Error json.RawMessage `json:"error"`
		}
		if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&response); e != nil {
			return Message{}, fmt.Errorf("模型响应解析失败: %w", e)
		}
		if len(response.Choices) == 0 {
			return Message{}, errors.New("模型未返回可用消息")
		}
		m := response.Choices[0].Message
		if e = validate(m); e != nil {
			return Message{}, e
		}
		if onText != nil && m.Content != "" {
			onText(m.Content)
		}
		return m, nil
	}
	scan := bufio.NewScanner(io.LimitReader(resp.Body, 2<<20))
	scan.Buffer(make([]byte, 8192), 256<<10)
	out := Message{Role: "assistant"}
	calls := map[int]*ToolCall{}
	done := false
	finished := false
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		if data == "" {
			continue
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int      `json:"index"`
						ID       string   `json:"id"`
						Type     string   `json:"type"`
						Function Function `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if e = json.Unmarshal([]byte(data), &chunk); e != nil {
			return Message{}, errors.New("模型流包含无效 JSON")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return Message{}, errors.New("模型流报告错误")
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0]
		if d.FinishReason != nil {
			finished = true
			if *d.FinishReason == "length" {
				return Message{}, errors.New("模型达到输出限制；未执行不完整工具调用")
			}
		}
		out.Content += d.Delta.Content
		out.ReasoningContent += d.Delta.ReasoningContent
		if len(out.Content) > 100000 || len(out.ReasoningContent) > 300000 {
			return Message{}, errors.New("模型输出超出安全上限")
		}
		if onText != nil && d.Delta.Content != "" {
			onText(d.Delta.Content)
		}
		for _, d := range d.Delta.ToolCalls {
			if d.Index < 0 || d.Index >= 16 {
				return Message{}, errors.New("单轮工具数量超限")
			}
			if calls[d.Index] == nil {
				calls[d.Index] = &ToolCall{Type: "function"}
			}
			t := calls[d.Index]
			if d.ID != "" {
				t.ID += d.ID
			}
			if d.Type != "" {
				t.Type = d.Type
			}
			t.Function.Name += d.Function.Name
			t.Function.Arguments += d.Function.Arguments
			if len(t.Function.Arguments) > 65536 || len(t.Function.Name) > 100 || len(t.ID) > 256 {
				return Message{}, errors.New("工具调用参数超限")
			}
		}
	}
	if e = scan.Err(); e != nil {
		return Message{}, fmt.Errorf("模型流中断: %w", e)
	}
	if !done || !finished {
		return Message{}, errors.New("模型流未完整结束；未执行工具调用")
	}
	indexes := []int{}
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		out.ToolCalls = append(out.ToolCalls, *calls[i])
	}
	if e = validate(out); e != nil {
		return Message{}, e
	}
	return out, nil
}
func validate(m Message) error {
	if m.Role != "assistant" {
		return errors.New("模型响应角色无效")
	}
	if len(m.ToolCalls) > 16 {
		return errors.New("工具调用数量超限")
	}
	ids := map[string]bool{}
	for _, t := range m.ToolCalls {
		if t.ID == "" || ids[t.ID] || t.Type != "function" || t.Function.Name == "" || !json.Valid([]byte(t.Function.Arguments)) {
			return errors.New("模型工具调用无效或参数不完整")
		}
		ids[t.ID] = true
	}
	if len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "" {
		return errors.New("模型返回空内容")
	}
	return nil
}
