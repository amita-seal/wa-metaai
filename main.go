// OpenAI-compatible shim that reaches Meta AI over WhatsApp.
//
// Uses whatsmeow rather than Baileys because Meta AI is a bot JID (867051314767696@bot) and bot
// delivery requires a <bot> stanza node plus an HKDF-derived BotMessageSecret. Baileys sends
// neither, so WhatsApp server-acks its messages and never delivers them (one grey tick forever).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3"
)

var (
	botJID    = types.NewMetaAIJID
	quietTime = envDuration("WA_QUIET_MS", 6000)
	graceTime = envDuration("WA_GRACE_MS", 18000)
	hardLimit = envDuration("WA_TIMEOUT_MS", 120000)
	modelID   = "meta-ai"
	debug     = os.Getenv("WA_DEBUG") == "1"
)

// messageKinds lists which fields a received message actually carries, so an unhandled shape shows
// up in the log instead of silently reading as empty text.
func messageKinds(m *waE2E.Message) string {
	if m == nil {
		return "<nil>"
	}
	raw, err := protojson.Marshal(m)
	if err != nil {
		return "?"
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return "?"
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func envDuration(key string, defMillis int) time.Duration {
	if v := os.Getenv(key); v != "" {
		var ms int
		if _, err := fmt.Sscanf(v, "%d", &ms); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return time.Duration(defMillis) * time.Millisecond
}

// collector gathers Meta AI's reply. It streams by editing one message with the full text so far,
// so entries are keyed by message ID and a later edit replaces the earlier fragment; separate
// messages still concatenate in arrival order. The answer is complete once quietTime passes with
// nothing new and the text no longer looks unfinished.
type collector struct {
	mu    sync.Mutex
	order []string
	texts map[string]string
	bump  chan struct{}
}

func newCollector() *collector {
	return &collector{texts: map[string]string{}, bump: make(chan struct{}, 64)}
}

func (c *collector) put(id, text string) {
	c.mu.Lock()
	if _, ok := c.texts[id]; !ok {
		c.order = append(c.order, id)
	}
	c.texts[id] = text
	c.mu.Unlock()
	select {
	case c.bump <- struct{}{}:
	default:
	}
}

func (c *collector) value() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, len(c.order))
	for _, id := range c.order {
		if t := c.texts[id]; t != "" {
			parts = append(parts, t)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func (c *collector) wait() string {
	deadline := time.After(hardLimit)
	// Meta AI composes in bursts and extends its answer by editing the same message, so a pause is
	// not the end. When the text still looks unfinished after the quiet window, wait out a bounded
	// grace period instead of the full hard limit, which would make every terse reply crawl.
	var grace <-chan time.Time
	for {
		select {
		case <-c.bump:
			grace = nil // more text arrived; the answer is still growing
		case <-time.After(quietTime):
			v := c.value()
			trunc := looksTruncated(v)
			if debug {
				log.Printf("~~ quiet elapsed len=%d truncated=%v grace=%v", len(v), trunc, grace != nil)
			}
			if !trunc {
				return v
			}
			if grace == nil {
				grace = time.After(graceTime)
			}
		case <-grace:
			return c.value()
		case <-deadline:
			return c.value()
		}
	}
}

// looksTruncated reports whether a reply is probably mid-composition: an unclosed ``` fence, an
// unbalanced JSON object, or prose that simply stops without any closing punctuation. The last case
// is what let "Here are some popular Python HTTP" through as if it were a finished answer.
func looksTruncated(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if strings.Count(s, "```")%2 != 0 {
		return true
	}
	if opens, closes := strings.Count(s, "{"), strings.Count(s, "}"); opens != closes {
		return true
	}
	// Decode the last rune rather than the last byte: replies routinely end in "…" or an emoji,
	// both multibyte, and a byte comparison would misread them.
	last, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsLetter(last) || unicode.IsDigit(last) || strings.ContainsRune(",;-—", last)
}

type bridge struct {
	client *whatsmeow.Client
	// One WhatsApp thread means one in-flight question; a global lock is the whole story.
	sendMu  sync.Mutex
	active  *collector
	activeM sync.RWMutex
}

func (b *bridge) handle(raw any) {
	msg, ok := raw.(*events.Message)
	if !ok {
		return
	}
	if msg.Info.IsFromMe || msg.Info.Chat.String() != botJID.String() {
		if debug {
			if m, ok := raw.(*events.Message); ok {
				log.Printf("~~ skipped chat=%s fromMe=%v", m.Info.Chat, m.Info.IsFromMe)
			}
		}
		return
	}
	id, text := extractReply(msg.Message, msg.Info.ID)
	if debug {
		log.Printf("~~ msg id=%s edit=%q len=%d kinds=%s", msg.Info.ID, msg.Info.Edit, len(text), messageKinds(msg.Message))
		if text == "" {
			if raw, err := protojson.Marshal(msg.Message); err == nil {
				body := string(raw)
				if len(body) > 900 {
					body = body[:900] + "…"
				}
				log.Printf("~~ empty-text payload: %s", body)
			}
		}
	}
	if text == "" {
		return
	}
	b.activeM.RLock()
	c := b.active
	b.activeM.RUnlock()
	if c != nil {
		c.put(id, text)
	}
}

// extractReply pulls the reply text out of a received message, and returns the collector key it
// belongs under.
//
// Meta AI streams by sending a short message and then repeatedly EDITING it with the full text so
// far. Those edits arrive as a protocolMessage of type MESSAGE_EDIT whose payload holds the whole
// answer, not a delta. whatsmeow only unwraps edits on its history-sync path, not on live messages,
// so reading conversation/extendedTextMessage alone sees empty text and the answer stays stuck at
// the first fragment. The edit's key ID is the original message's ID, so keying on it replaces the
// fragment instead of appending a duplicate.
func extractReply(m *waE2E.Message, fallbackID string) (id string, text string) {
	id = fallbackID
	if pm := m.GetProtocolMessage(); pm.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT {
		if key := pm.GetKey().GetID(); key != "" {
			id = key
		}
		if edited := pm.GetEditedMessage(); edited != nil {
			if t := edited.GetConversation(); t != "" {
				return id, t
			}
			if t := edited.GetExtendedTextMessage().GetText(); t != "" {
				return id, t
			}
		}
	}
	if t := m.GetConversation(); t != "" {
		return id, t
	}
	return id, m.GetExtendedTextMessage().GetText()
}

func (b *bridge) ask(ctx context.Context, prompt string) (string, error) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()

	c := newCollector()
	b.activeM.Lock()
	b.active = c
	b.activeM.Unlock()
	defer func() {
		b.activeM.Lock()
		b.active = nil
		b.activeM.Unlock()
	}()

	_, err := b.client.SendMessage(ctx, botJID, &waE2E.Message{Conversation: proto.String(prompt)})
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", botJID, err)
	}
	reply := c.wait()
	if reply == "" {
		return "", fmt.Errorf("no reply from %s within %s", botJID, hardLimit)
	}
	return reply, nil
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []toolCall      `json:"tool_calls"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatReq struct {
	Messages []chatMsg `json:"messages"`
	Tools    []toolDef `json:"tools"`
	Stream   bool      `json:"stream"`
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// Meta AI has no native function calling, so tool use is prompted: the schemas go into the text and
// a tool call comes back as a fenced JSON object which parseToolCall extracts. Kept deliberately
// blunt — a consumer assistant ignores elaborate instructions more often than terse ones.
func toolPrompt(tools []toolDef) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[TOOLS]\nYou can run tools. Available tools:\n")
	for _, t := range tools {
		params := strings.TrimSpace(string(t.Function.Parameters))
		if params == "" {
			params = "{}"
		}
		fmt.Fprintf(&b, "\n- %s: %s\n  parameters (JSON Schema): %s\n",
			t.Function.Name, t.Function.Description, params)
	}
	b.WriteString(`
To run a tool, reply with ONLY this and nothing else, no prose before or after:
` + "```json" + `
{"tool": "<tool name>", "args": {<arguments matching the schema>}}
` + "```" + `
You will then be given the tool's output as [TOOL RESULT] and may run another tool or answer.
If you do not need a tool, just answer normally.

Rules for file paths: write files to the project working directory using a RELATIVE path such as
"server.js". Never write into a temporary directory, and ignore any temp directory path mentioned in
a tool description above unless the user explicitly asked for it.

Rules for long-running commands: anything that does not exit on its own, such as starting a server,
must be backgrounded and its output redirected, e.g. "node server.js > /tmp/srv.log 2>&1 &". Running
it in the foreground makes the tool call time out and the process is then killed, so a follow-up
curl finds nothing listening.

Rules for the shell: the bash session is persistent and already starts in the correct project
directory, so never use "cd". A "cd" leaks into later commands and makes relative paths write to the
wrong place.

Rules for finishing: NEVER repeat a tool call that already appears above with its [TOOL RESULT].
When the results above already contain what the user asked for, you are done: reply with the final
answer as plain text, with no JSON and no tool call. Verifying the same thing twice is a mistake.
`)
	return b.String()
}

func flatten(r chatReq) string {
	var turns []string
	if tp := toolPrompt(r.Tools); tp != "" {
		turns = append(turns, tp)
	}
	for _, m := range r.Messages {
		// An assistant turn that called a tool carries no content; replay the call so Meta AI can
		// see what it already asked for and not loop on the same tool.
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				turns = append(turns, fmt.Sprintf("[ASSISTANT ran tool]\n{\"tool\": %q, \"args\": %s}",
					tc.Function.Name, orEmptyJSON(tc.Function.Arguments)))
			}
			continue
		}
		t := contentText(m.Content)
		if strings.TrimSpace(t) == "" {
			continue
		}
		label := strings.ToUpper(m.Role)
		if m.Role == "tool" {
			label = "TOOL RESULT"
		}
		turns = append(turns, fmt.Sprintf("[%s]\n%s", label, t))
	}
	if len(turns) == 0 {
		return ""
	}
	return strings.Join(turns, "\n\n") + "\n\n[ASSISTANT]"
}

var (
	// opencode advertises a scratch directory in its bash tool description, and Meta AI treats that
	// as where the user's files belong no matter how firmly the prompt says otherwise. Rewriting the
	// arguments is the only fix that does not depend on it following instructions.
	tempDirPath = regexp.MustCompile(`(?:/private)?/var/folders/[^/\s"']+/[^/\s"']+/T/opencode/?`)
	leadingCD   = regexp.MustCompile(`^\s*cd\s+(?:'[^']*'|"[^"]*"|\S+)\s*&&\s*`)
	pathKeys    = map[string]bool{"filePath": true, "path": true, "file": true, "filename": true}
)

// sanitizeArgs keeps tool calls inside the project directory: absolute paths into opencode's temp
// scratch directory collapse to a bare filename, and a leading "cd ... &&" is dropped because the
// bash session is persistent and already in the right place.
func sanitizeArgs(args string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return args
	}
	changed := false
	for k, v := range obj {
		s, ok := v.(string)
		if !ok {
			continue
		}
		orig := s
		if pathKeys[k] && tempDirPath.MatchString(s) {
			s = path.Base(s)
		}
		if k == "command" || k == "workdir" {
			s = leadingCD.ReplaceAllString(s, "")
			s = tempDirPath.ReplaceAllString(s, "")
			if k == "workdir" && strings.TrimSpace(s) == "" {
				delete(obj, k)
				changed = true
				continue
			}
		}
		if s != orig {
			obj[k] = s
			changed = true
		}
	}
	if !changed {
		return args
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return args
	}
	return string(out)
}

// canonicalJSON normalises argument formatting so "{\"a\":1}" and "{ \"a\": 1 }" compare equal.
func canonicalJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return strings.Join(strings.Fields(s), "")
	}
	out, err := json.Marshal(v)
	if err != nil {
		return strings.Join(strings.Fields(s), "")
	}
	return string(out)
}

// priorCallCount reports how many times this exact tool call already appears in the transcript.
// Meta AI will happily re-run a command it just ran, forever, so the shim has to notice.
func priorCallCount(msgs []chatMsg, name, args string) int {
	want := canonicalJSON(args)
	n := 0
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == name && canonicalJSON(orEmptyJSON(tc.Function.Arguments)) == want {
				n++
			}
		}
	}
	return n
}

// stripToolJSON removes a fenced tool-call block from a reply that is being downgraded to plain
// text, so the user never sees raw {"tool":...} JSON as their answer.
func stripToolJSON(s string) string {
	if fenced := betweenFences(s); fenced != "" && strings.Contains(fenced, `"tool"`) {
		if i := strings.Index(s, "```"); i >= 0 {
			rest := s[i+3:]
			if j := strings.Index(rest, "```"); j >= 0 {
				s = strings.TrimSpace(s[:i] + rest[j+3:])
			} else {
				s = strings.TrimSpace(s[:i])
			}
		}
	}
	if s == "" {
		return "Done."
	}
	return s
}

const stopToolsInstruction = `
[IMPORTANT] You just asked to run a tool call that already ran, and its result is in the transcript
above. Do NOT output any tool call or JSON now. Reply with your final answer to the user as plain
text.`

func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// parseToolCall pulls a {"tool":..,"args":..} object out of Meta AI's reply. It tolerates the
// model wrapping the JSON in a fence or in chatter, which it frequently does.
func parseToolCall(reply string) (name string, args string, ok bool) {
	candidates := []string{}
	if fenced := betweenFences(reply); fenced != "" {
		candidates = append(candidates, fenced)
	}
	if start := strings.Index(reply, "{"); start >= 0 {
		if end := strings.LastIndex(reply, "}"); end > start {
			candidates = append(candidates, reply[start:end+1])
		}
	}
	for _, c := range candidates {
		var probe struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal([]byte(c), &probe); err != nil || probe.Tool == "" {
			continue
		}
		return probe.Tool, orEmptyJSON(string(probe.Args)), true
	}
	return "", "", false
}

func betweenFences(s string) string {
	i := strings.Index(s, "```")
	if i < 0 {
		return ""
	}
	rest := s[i+3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:] // drop an optional language tag such as ```json
	}
	if j := strings.Index(rest, "```"); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return strings.TrimSpace(rest)
}

func tokens(s string) int { return (len(s) + 3) / 4 }

func (b *bridge) serveChat(w http.ResponseWriter, r *http.Request) {
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prompt := flatten(req)
	if prompt == "" {
		http.Error(w, "no prompt content", http.StatusBadRequest)
		return
	}
	log.Printf("-> %s", strings.ReplaceAll(prompt, "\n", " | "))
	reply, err := b.ask(r.Context(), prompt)
	if err != nil {
		log.Printf("!! %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	log.Printf("<- %s", strings.ReplaceAll(reply, "\n", " | "))

	created := time.Now().Unix()
	name, args, isCall := "", "", false
	if len(req.Tools) > 0 {
		name, args, isCall = parseToolCall(reply)
		if isCall {
			if clean := sanitizeArgs(args); clean != args {
				log.Printf("== rewrote args %s -> %s", args, clean)
				args = clean
			}
		}
		// Meta AI loops: having run a command successfully it will re-run it indefinitely instead of
		// answering. If it asks for a call already in the transcript, demand a plain-text answer
		// instead, and if it still insists on a tool call, drop the tool call so the agent
		// terminates with whatever text it produced.
		if isCall && priorCallCount(req.Messages, name, args) > 0 {
			log.Printf("== repeat of %s %s — forcing a final answer", name, args)
			if retry, rerr := b.ask(r.Context(), prompt+stopToolsInstruction); rerr == nil && retry != "" {
				reply = retry
				if n2, a2, ok2 := parseToolCall(retry); ok2 && priorCallCount(req.Messages, n2, a2) == 0 {
					name, args, isCall = n2, a2, ok2
				} else {
					name, args, isCall = "", "", false
					reply = stripToolJSON(retry)
				}
			} else {
				name, args, isCall = "", "", false
				reply = stripToolJSON(reply)
			}
		}
		if isCall {
			log.Printf("== tool call: %s %s", name, args)
		}
	}
	callID := "call_" + msgID()

	if !req.Stream {
		message := map[string]any{"role": "assistant", "content": reply}
		finish := "stop"
		if isCall {
			finish = "tool_calls"
			message = map[string]any{"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{"id": callID, "type": "function",
					"function": map[string]string{"name": name, "arguments": args}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-" + msgID(), "object": "chat.completion", "created": created, "model": modelID,
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
			"usage": map[string]int{"prompt_tokens": tokens(prompt), "completion_tokens": tokens(reply),
				"total_tokens": tokens(prompt) + tokens(reply)},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, finish any) {
		payload, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-" + msgID(), "object": "chat.completion.chunk", "created": created, "model": modelID,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk(map[string]any{"role": "assistant"}, nil)
	if isCall {
		chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": callID,
			"type": "function", "function": map[string]string{"name": name, "arguments": args}}}}, nil)
		chunk(map[string]any{}, "tool_calls")
	} else {
		chunk(map[string]any{"content": reply}, nil)
		chunk(map[string]any{}, "stop")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func msgID() string { return whatsmeow.GenerateMessageID() }

func main() {
	ctx := context.Background()
	dbLog := waLog.Stdout("db", "WARN", true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:wametaai.db?_foreign_keys=on", dbLog)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("device: %v", err)
	}
	client := whatsmeow.NewClient(device, waLog.Stdout("wa", "INFO", true))
	b := &bridge{client: client}
	client.AddEventHandler(b.handle)

	if client.Store.ID == nil {
		phone := os.Getenv("WA_PHONE")
		if phone == "" {
			log.Fatal("not linked yet: set WA_PHONE=<digits, country code first>")
		}
		qrChan, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
			log.Fatalf("connect: %v", err)
		}
		go func() {
			for range qrChan {
				// Draining the QR channel is required even when pairing by phone code.
			}
		}()
		code, err := client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (MacOS)")
		if err != nil {
			log.Fatalf("pair: %v", err)
		}
		log.Printf("\n\n  WhatsApp > Linked devices > Link with phone number\n  pairing code: %s\n\n", code)
	} else if err := client.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}

	http.HandleFunc("/v1/chat/completions", b.serveChat)
	http.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
			map[string]any{"id": modelID, "object": "model", "created": 0, "owned_by": "meta"}}})
	})

	port := os.Getenv("WA_PORT")
	if port == "" {
		port = "8788"
	}
	log.Printf("listening on http://localhost:%s/v1  target=%s", port, botJID)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
