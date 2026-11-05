package onebot

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// wireID never decodes through float64. Both JSON int64 and string IDs are
// accepted; decimals/exponents are rejected instead of silently losing bits.
type wireID string

func (id *wireID) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return errors.New("onebot: missing identifier")
	}
	var value string
	if raw[0] == '"' {
		if json.Unmarshal(raw, &value) != nil {
			return errors.New("onebot: invalid identifier")
		}
	} else {
		value = string(raw)
	}
	if !validMessageID(value) {
		return errors.New("onebot: identifier must be a canonical decimal integer")
	}
	*id = wireID(value)
	return nil
}

func validMessageID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && strconv.FormatInt(n, 10) == s
}

func validAccountID(s string) bool {
	return validMessageID(s) && s[0] != '-' && s != "0"
}

type textData struct {
	Text string `json:"text"`
}

type textSegment struct {
	Type string   `json:"type"`
	Data textData `json:"data"`
}

func parseEvent(body []byte, expectedBot string) (Event, bool, error) {
	var e Event
	if len(body) > MaxBodyBytes || !utf8.Valid(body) {
		return e, false, ErrProtocol
	}
	var envelope struct {
		PostType string `json:"post_type"`
		SelfID   wireID `json:"self_id"`
	}
	if json.Unmarshal(body, &envelope) != nil || !validAccountID(string(envelope.SelfID)) {
		return e, false, ErrProtocol
	}
	// Validate even notice/meta/message_sent envelopes: a live connection must
	// not silently switch QQ accounts after its initial get_login_info check.
	if string(envelope.SelfID) != expectedBot {
		return e, false, ErrBotIDMismatch
	}
	if envelope.PostType != "message" {
		return e, false, nil
	}
	var message struct {
		MessageType string          `json:"message_type"`
		SubType     string          `json:"sub_type"`
		MessageID   wireID          `json:"message_id"`
		UserID      wireID          `json:"user_id"`
		GroupID     wireID          `json:"group_id"`
		Message     json.RawMessage `json:"message"`
		Anonymous   json.RawMessage `json:"anonymous"`
	}
	if json.Unmarshal(body, &message) != nil {
		return e, false, ErrProtocol
	}
	if message.MessageType != "private" && message.MessageType != "group" {
		return e, false, nil
	}
	if !validAccountID(string(message.UserID)) || !validMessageID(string(message.MessageID)) {
		return e, false, ErrProtocol
	}
	if string(message.UserID) == expectedBot {
		return e, false, nil
	}
	isGroup := message.MessageType == "group"
	if isGroup {
		if !validAccountID(string(message.GroupID)) {
			return e, false, ErrProtocol
		}
		// Anonymous/system notices do not carry a trustworthy user identity for
		// an operations allowlist, even when sender.user_id looks like a user.
		if message.SubType == "anonymous" || message.SubType == "notice" || len(message.Anonymous) > 0 && !bytes.Equal(bytes.TrimSpace(message.Anonymous), []byte("null")) {
			return e, false, nil
		}
	}
	text, supported, mentioned, err := plainText(message.Message, expectedBot, isGroup)
	if err != nil {
		return e, false, err
	}
	if !supported || strings.TrimSpace(text) == "" {
		return e, false, nil
	}
	e = Event{
		BotID: expectedBot, UserID: string(message.UserID),
		MessageID: string(message.MessageID), Text: strings.TrimSpace(text), IsGroup: isGroup, Mentioned: mentioned,
	}
	// A message_id can recur across chats and across bot accounts. Keep those
	// namespaces in the stable router ID while preserving the original ID.
	if isGroup {
		e.GroupID = string(message.GroupID)
		e.ID = expectedBot + ":group:" + e.GroupID + ":" + e.MessageID
	} else {
		e.ID = expectedBot + ":private:" + e.UserID + ":" + e.MessageID
	}
	return e, true, nil
}

// Only textual messages are accepted. A leading mention of THIS bot may be
// removed in group chat; other CQ/media/reply/forward segments drop the entire
// message, rather than joining fragments into a different executable command.
// No content, URL, file path or script inside any segment is ever evaluated.
func plainText(raw json.RawMessage, botID string, isGroup bool) (text string, supported, mentioned bool, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false, false, ErrProtocol
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return "", false, false, ErrProtocol
		}
		text = strings.TrimSpace(text)
		if isGroup {
			prefix := "[CQ:at,qq=" + botID + "]"
			if strings.HasPrefix(text, prefix) {
				mentioned = true
				text = strings.TrimLeft(text[len(prefix):], " \t\r\n")
			}
		}
		// Inspect before entity decoding. Escaped literal CQ-like text remains
		// literal and must not be interpreted a second time.
		if strings.Contains(text, "[CQ:") {
			return "", false, false, nil
		}
		text = cqTextUnescape.Replace(text)
		if len(text) > MaxTextBytes {
			return "", false, false, ErrProtocol
		}
		return text, true, mentioned, nil
	}
	if raw[0] != '[' {
		return "", false, false, ErrProtocol
	}
	var segments []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &segments) != nil || len(segments) > 256 {
		return "", false, false, ErrProtocol
	}
	var out strings.Builder
	for _, segment := range segments {
		switch segment.Type {
		case "text":
			var data textData
			if json.Unmarshal(segment.Data, &data) != nil {
				return "", false, false, ErrProtocol
			}
			if out.Len()+len(data.Text) > MaxTextBytes {
				return "", false, false, ErrProtocol
			}
			// Array-format text is already plain; do not CQ-unescape it.
			out.WriteString(data.Text)
		case "at":
			if !isGroup || mentioned || strings.TrimSpace(out.String()) != "" {
				return "", false, false, nil
			}
			var data struct {
				QQ wireID `json:"qq"`
			}
			if json.Unmarshal(segment.Data, &data) != nil || string(data.QQ) != botID {
				return "", false, false, nil
			}
			mentioned = true
		default:
			return "", false, false, nil
		}
	}
	return out.String(), true, mentioned, nil
}

// strings.Replacer is deliberately single-pass: &amp;#91; remains &#91;,
// rather than recursively turning into a CQ opening bracket.
var cqTextUnescape = strings.NewReplacer("&amp;", "&", "&#91;", "[", "&#93;", "]")
