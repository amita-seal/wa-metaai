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

func TestPriorCallCountIgnoresFormatting(t *testing.T) {
	var req chatReq
	body := `{"messages":[
	  {"role":"assistant","tool_calls":[{"function":{"name":"bash","arguments":"{\"command\":\"cat hello.txt\"}"}}]},
	  {"role":"tool","content":"hello world"},
	  {"role":"assistant","tool_calls":[{"function":{"name":"bash","arguments":"{ \"command\" : \"cat hello.txt\" }"}}]}
	]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if n := priorCallCount(req.Messages, "bash", `{"command":"cat hello.txt"}`); n != 2 {
		t.Errorf("whitespace-different args should still match: got %d want 2", n)
	}
	if n := priorCallCount(req.Messages, "bash", `{"command":"ls"}`); n != 0 {
		t.Errorf("different command should not match: got %d", n)
	}
	if n := priorCallCount(req.Messages, "write", `{"command":"cat hello.txt"}`); n != 0 {
		t.Errorf("different tool should not match: got %d", n)
	}
}

func TestStripToolJSON(t *testing.T) {
	in := "Sure!\n```json\n{\"tool\":\"bash\",\"args\":{\"command\":\"ls\"}}\n```\nAll set."
	got := stripToolJSON(in)
	if strings.Contains(got, `"tool"`) {
		t.Errorf("tool JSON survived: %q", got)
	}
	if !strings.Contains(got, "All set.") {
		t.Errorf("surrounding prose lost: %q", got)
	}
	if stripToolJSON("```json\n{\"tool\":\"x\",\"args\":{}}\n```") != "Done." {
		t.Error("a reply that is only tool JSON should degrade to a placeholder")
	}
}

func TestSanitizeArgsKeepsWorkInsideProject(t *testing.T) {
	tmp := "/var/folders/sp/dpt45v7104lc76v27crlqzbm0000gn/T/opencode"
	cases := []struct{ label, in, want string }{
		{"abs temp filePath", `{"filePath":"` + tmp + `/sum.py","content":"x"}`, `{"content":"x","filePath":"sum.py"}`},
		{"cd prefix stripped", `{"command":"cd ` + tmp + ` && touch sum.py"}`, `{"command":"touch sum.py"}`},
		{"temp path inside command", `{"command":"python ` + tmp + `/sum.py"}`, `{"command":"python sum.py"}`},
		// Unchanged input is returned verbatim rather than re-marshalled, so key order survives.
		{"relative left alone", `{"filePath":"hello.txt","content":"hi"}`, `{"filePath":"hello.txt","content":"hi"}`},
		{"unrelated abs path kept", `{"filePath":"/etc/hosts"}`, `{"filePath":"/etc/hosts"}`},
		{"malformed json untouched", `not json`, `not json`},
	}
	for _, c := range cases {
		got := sanitizeArgs(c.in)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.label, got, c.want)
		}
	}
	// A workdir that was only the temp dir should disappear rather than become empty.
	if got := sanitizeArgs(`{"command":"ls","workdir":"` + tmp + `"}`); strings.Contains(got, "workdir") {
		t.Errorf("empty workdir should be dropped, got %s", got)
	}
}

func TestSanitizeArgsDropsEmptyMkdir(t *testing.T) {
	tmp := "/var/folders/sp/dpt45v7104lc76v27crlqzbm0000gn/T/opencode"
	got := sanitizeArgs(`{"command":"mkdir -p ` + tmp + ` && touch sum.py"}`)
	if strings.Contains(got, "mkdir") {
		t.Errorf("empty mkdir should be removed, got %s", got)
	}
	if !strings.Contains(got, "touch sum.py") {
		t.Errorf("rest of the command must survive, got %s", got)
	}
	// A legitimate mkdir with a real operand must be left alone.
	keep := sanitizeArgs(`{"command":"mkdir -p build && touch build/x"}`)
	if !strings.Contains(keep, "mkdir -p build") {
		t.Errorf("real mkdir was damaged: %s", keep)
	}
}

func TestUnrequestedDeletion(t *testing.T) {
	parse := func(body string) chatReq {
		var r chatReq
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	// The observed failure: it created the file with plain shell redirection, not the write tool,
	// then deleted it. Tracking write-tool paths alone missed this.
	create := parse(`{"messages":[{"role":"user","content":"Create a python file that prints 5 and run it"}]}`)
	if !unrequestedDeletion(create.Messages, "bash", `{"command":"rm sum.py"}`) {
		t.Error("rm must be refused when the user never asked to delete anything")
	}
	if unrequestedDeletion(create.Messages, "bash", `{"command":"python sum.py"}`) {
		t.Error("running a file is not a deletion")
	}
	if unrequestedDeletion(create.Messages, "bash", `{"command":"echo confirm"}`) {
		t.Error("'confirm' contains rm as a substring and must not match")
	}
	if unrequestedDeletion(create.Messages, "write", `{"command":"rm x"}`) {
		t.Error("only bash calls delete things")
	}

	// When deletion is what was asked for, allow it.
	for _, ask := range []string{"delete the temp files", "please remove build/", "clean up the logs"} {
		req := parse(`{"messages":[{"role":"user","content":"` + ask + `"}]}`)
		if unrequestedDeletion(req.Messages, "bash", `{"command":"rm -rf build"}`) {
			t.Errorf("deletion asked for via %q must be allowed", ask)
		}
	}
}

func TestSanitizeArgsStripsInventedScratchDir(t *testing.T) {
	// Observed: Meta AI invented a relative "opencode/" directory from the scratch path named in
	// opencode's own tool description, so the file landed one level down and the task failed.
	if got := sanitizeArgs(`{"filePath":"opencode/sum.py","content":"x"}`); !strings.Contains(got, `"filePath":"sum.py"`) {
		t.Errorf("relative scratch prefix not stripped: %s", got)
	}
	got := sanitizeArgs(`{"command":"mkdir -p opencode && touch opencode/sum.py"}`)
	if strings.Contains(got, "opencode") {
		t.Errorf("scratch dir survived in command: %s", got)
	}
	if !strings.Contains(got, "touch sum.py") {
		t.Errorf("rest of command damaged: %s", got)
	}
	if got := sanitizeArgs(`{"command":"python opencode/sum.py"}`); !strings.Contains(got, "python sum.py") {
		t.Errorf("run command not corrected: %s", got)
	}
	// A path merely containing the word must survive.
	if got := sanitizeArgs(`{"filePath":"docs/opencode-notes.md"}`); !strings.Contains(got, "docs/opencode-notes.md") {
		t.Errorf("unrelated path damaged: %s", got)
	}
}

func TestToolPromptRedactsScratchPath(t *testing.T) {
	var tools []toolDef
	body := `[{"type":"function","function":{"name":"bash","description":"Use /var/folders/sp/abc/xyz/T/opencode for temporary work outside the workspace.","parameters":{}}}]`
	if err := json.Unmarshal([]byte(body), &tools); err != nil {
		t.Fatal(err)
	}
	out := toolPrompt(tools)
	if strings.Contains(out, "/var/folders") {
		t.Errorf("scratch path should not reach the model: %s", out)
	}
	if !strings.Contains(out, "the working directory") {
		t.Errorf("expected replacement text, got: %s", out)
	}
}
