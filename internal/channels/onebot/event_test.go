package onebot

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestWireIDExactIntegers(t *testing.T) {
	for _, raw := range []string{`9007199254740993`, `"9007199254740993"`, `9223372036854775807`, `-2147483648`, `0`} {
		var id wireID
		if err := json.Unmarshal([]byte(raw), &id); err != nil || string(id) != strings.Trim(raw, `"`) {
			t.Fatalf("lost exact identifier %s: %s, %v", raw, id, err)
		}
	}
	for _, raw := range []string{`9007199254740993.0`, `9e15`, `"01"`, `"-0"`, `null`, `true`, `9223372036854775808`, `""`, `"+1"`} {
		var id wireID
		if err := json.Unmarshal([]byte(raw), &id); err == nil {
			t.Fatalf("accepted noncanonical identifier %s", raw)
		}
	}
}

func TestParseExactIDsAndMentions(t *testing.T) {
	const botID = "9007199254740993"
	tests := []struct {
		name, message, wantText string
		mention                 bool
	}{
		{"string", `"/status"`, "/status", false},
		{"cq-mention", `"[CQ:at,qq=9007199254740993] /status"`, "/status", true},
		{"array-mention", `[{"type":"at","data":{"qq":"9007199254740993"}},{"type":"text","data":{"text":" /status"}}]`, "/status", true},
		{"array-text", `[{"type":"text","data":{"text":"/status"}}]`, "/status", false},
		{"escaped-mention-not-real", `"&#91;CQ:at,qq=9007199254740993&#93; /status"`, "[CQ:at,qq=9007199254740993] /status", false},
		{"text-segment-not-mention", `[{"type":"text","data":{"text":"[CQ:at,qq=9007199254740993] /status"}}]`, "[CQ:at,qq=9007199254740993] /status", false},
		{"single-pass-unescape", `"&amp;#91;CQ:image&#93; &amp; &lt; &#44;"`, "&#91;CQ:image] & &lt; &#44;", false},
		{"array-does-not-unescape", `[{"type":"text","data":{"text":"&amp; &#91;"}}]`, "&amp; &#91;", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"post_type":"message","self_id":%s,"message_type":"group","sub_type":"normal","user_id":9007199254740995,"group_id":"9007199254740997","message_id":-2147483648,"message":%s}`, botID, tc.message)
			e, ok, err := parseEvent([]byte(body), botID)
			if err != nil || !ok || e.BotID != botID || e.UserID != "9007199254740995" || e.GroupID != "9007199254740997" || e.MessageID != "-2147483648" || e.Text != tc.wantText || e.Mentioned != tc.mention || !e.IsGroup {
				t.Fatalf("event: %#v, supported=%v, err=%v", e, ok, err)
			}
		})
	}
}

func TestParseIgnoresNonTextAndOwnMessages(t *testing.T) {
	for _, message := range []string{
		`"[CQ:image,file=file:///do-not-open] /status"`,
		`"/sta[CQ:image,file=x]tus"`,
		`"[CQ:at,qq=999] /status"`,
		`"[CQ:reply,id=1][CQ:at,qq=123] /status"`,
		`[{"type":"image","data":{"file":"file:///do-not-open"}},{"type":"text","data":{"text":"/status"}}]`,
		`[{"type":"reply","data":{"id":1}},{"type":"text","data":{"text":"/status"}}]`,
		`[{"type":"at","data":{"qq":"all"}},{"type":"text","data":{"text":"/status"}}]`,
		`[{"type":"json","data":{"data":"{\"code\":\"do not run\"}"}}]`,
	} {
		body := fmt.Sprintf(`{"post_type":"message","self_id":123,"message_type":"group","sub_type":"normal","user_id":456,"group_id":789,"message_id":1,"message":%s}`, message)
		if _, ok, err := parseEvent([]byte(body), "123"); err != nil || ok {
			t.Fatalf("non-text should be ignored: %s, %v, %v", message, ok, err)
		}
	}
	for _, body := range []string{
		`{"post_type":"message_sent","self_id":123}`,
		`{"post_type":"meta_event","self_id":123}`,
		`{"post_type":"message","self_id":123,"message_type":"private","user_id":123,"message_id":1,"message":"/status"}`,
		`{"post_type":"message","self_id":123,"message_type":"group","sub_type":"anonymous","user_id":456,"group_id":789,"message_id":1,"message":"/status"}`,
	} {
		if _, ok, err := parseEvent([]byte(body), "123"); err != nil || ok {
			t.Fatalf("ignored event became a command: %v %v", ok, err)
		}
	}
}

func TestParseFailClosedAndLimits(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"post_type":"message","self_id":123} {}`,
		`{"post_type":"message","self_id":1e2}`,
		`{"post_type":"message","self_id":123,"message_type":"private","user_id":456,"message":"/status"}`,
		`{"post_type":"message","self_id":123,"message_type":"private","user_id":456,"message_id":1.0,"message":"/status"}`,
	} {
		if _, _, err := parseEvent([]byte(body), "123"); err == nil {
			t.Fatalf("accepted invalid event: %s", body)
		}
	}
	if _, _, err := parseEvent([]byte(`{"post_type":"meta_event","self_id":999}`), "123"); !errors.Is(err, ErrBotIDMismatch) {
		t.Fatalf("account swap not detected: %v", err)
	}
	if _, _, err := parseEvent(make([]byte, MaxBodyBytes+1), "123"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversized event: %v", err)
	}
	large, _ := json.Marshal(strings.Repeat("a", MaxTextBytes+1))
	if _, _, _, err := plainText(large, "123", false); !errors.Is(err, ErrProtocol) {
		t.Fatal("oversized text accepted")
	}
}
