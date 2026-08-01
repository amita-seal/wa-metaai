package main

import (
	"encoding/json"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestParseToolCall(t *testing.T) {
	cases := []struct {
		label string
		reply string
		name  string
		args  string
		ok    bool
	}{
		{"bare json", `{"tool":"read","args":{"filePath":"a.go"}}`, "read", `{"filePath":"a.go"}`, true},
		{"fenced with lang", "```json\n{\"tool\":\"bash\",\"args\":{\"command\":\"ls\"}}\n```", "bash", `{"command":"ls"}`, true},
		{"fenced no lang", "```\n{\"tool\":\"ls\",\"args\":{}}\n```", "ls", `{}`, true},
		// Meta AI habitually adds chatter around the JSON despite being told not to.
		{"wrapped in prose", "Sure! Here you go:\n```json\n{\"tool\":\"glob\",\"args\":{\"pattern\":\"*.go\"}}\n```\nHope that helps!", "glob", `{"pattern":"*.go"}`, true},
		{"missing args", `{"tool":"list"}`, "list", `{}`, true},
		{"plain prose", "The capital of France is Paris.", "", "", false},
		{"json without tool key", `{"answer":"42"}`, "", "", false},
	}
	for _, c := range cases {
		name, args, ok := parseToolCall(c.reply)
		if ok != c.ok || name != c.name {
			t.Errorf("%s: got (%q,%q,%v) want (%q,%q,%v)", c.label, name, args, ok, c.name, c.args, c.ok)
			continue
		}
		if ok && !json.Valid([]byte(args)) {
			t.Errorf("%s: args not valid JSON: %s", c.label, args)
		}
	}
}

func TestFlattenIncludesToolsAndResults(t *testing.T) {
	var req chatReq
	body := `{"messages":[
	  {"role":"user","content":"read a.go"},
	  {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"filePath\":\"a.go\"}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":"package main"}
	],"tools":[{"type":"function","function":{"name":"read","description":"Read a file","parameters":{"type":"object"}}}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	out := flatten(req)
	for _, want := range []string{"[TOOLS]", "read: Read a file", "[ASSISTANT ran tool]", "[TOOL RESULT]", "package main", "[ASSISTANT]"} {
		if !strings.Contains(out, want) {
			t.Errorf("flatten output missing %q\n---\n%s", want, out)
		}
	}
}

func TestToolPromptEmptyWithoutTools(t *testing.T) {
	if toolPrompt(nil) != "" {
		t.Error("expected no tool prompt when no tools are offered")
	}
}

func TestLooksTruncated(t *testing.T) {
	cases := []struct {
		label string
		text  string
		want  bool
	}{
		{"empty", "", true},
		{"plain prose", "The capital of France is Paris.", false},
		{"closed fence", "```json\n{\"tool\":\"read\",\"args\":{}}\n```", false},
		{"unclosed fence", "```json\n{\"tool\":\"write\",\"args\":{\"content\":\"const http", true},
		// The shapes that got returned as if finished, truncating visible output.
		{"cut mid word", "Node HTTP server responding with n", true},
		{"cut mid sentence", "Here are some popular Python HTTP", true},
		{"unbalanced braces", `{"tool":"write","args":{"content":"x"`, true},
		{"balanced braces in prose", "use {} for an empty object.", false},
		{"ends with ellipsis", "Still working on it…", false},
		{"ends with emoji", "All done 🎉", false},
		{"trailing comma means more", "First, we install the deps,", true},
		{"bare one word answer", "pomegranate", true},
	}
	for _, c := range cases {
		if got := looksTruncated(c.text); got != c.want {
			t.Errorf("%s: looksTruncated(%q)=%v want %v", c.label, c.text, got, c.want)
		}
	}
}

func TestExtractReplyUnwrapsStreamingEdits(t *testing.T) {
	// Plain first fragment.
	plain := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("A reverse proxy is a server")}}
	id, text := extractReply(plain, "MSG1")
	if id != "MSG1" || text != "A reverse proxy is a server" {
		t.Fatalf("plain: got (%q,%q)", id, text)
	}

	// The streaming edit: full text inside protocolMessage, keyed at the original message ID.
	full := "A reverse proxy is a server that sits between clients and servers."
	edit := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("MSG1")},
		EditedMessage: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(full)},
		},
	}}
	id2, text2 := extractReply(edit, "SOME_OTHER_EVENT_ID")
	if id2 != "MSG1" {
		t.Errorf("edit should key on the original message ID, got %q", id2)
	}
	if text2 != full {
		t.Errorf("edit text: got %q want %q", text2, full)
	}

	// Same key means the collector replaces rather than concatenates.
	c := newCollector()
	c.put(id, text)
	c.put(id2, text2)
	if got := c.value(); got != full {
		t.Errorf("collector should hold only the full text, got %q", got)
	}
}
