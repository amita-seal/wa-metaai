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
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3"
)

var (
	botJID    = types.NewMetaAIJID
	quietTime = envDuration("WA_QUIET_MS", 6000)
	hardLimit = envDuration("WA_TIMEOUT_MS", 120000)
	modelID   = "meta-ai"
)

func envDuration(key string, defMillis int) time.Duration {
	if v := os.Getenv(key); v != "" {
		var ms int
		if _, err := fmt.Sscanf(v, "%d", &ms); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return time.Duration(defMillis) * time.Millisecond
}

// collector gathers Meta AI's reply. It arrives as many separate messages rather than edits to
// one, so replies are keyed by message ID and joined in arrival order; the answer is complete
// once quietTime passes with nothing new.
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
	for {
		select {
		case <-c.bump:
			// keep waiting: more chunks are still arriving
		case <-time.After(quietTime):
			// Meta AI composes long answers in bursts and can pause mid-code-block for longer than
			// the quiet window. Returning then yields a fragment, and a fragment of a tool call is
			// unparseable JSON, which looks like the agent hanging. Keep waiting while the text is
			// visibly unfinished.
			if v := c.value(); !looksTruncated(v) {
				return v
			}
		case <-deadline:
			return c.value()
		}
	}
}

// looksTruncated reports whether a reply is obviously mid-composition: an unclosed ``` fence, or an
// unbalanced JSON object, both of which mean more text is still coming.
func looksTruncated(s string) bool {
	if s == "" {
		return true
	}
	if strings.Count(s, "```")%2 != 0 {
		return true
	}
	if opens, closes := strings.Count(s, "{"), strings.Count(s, "}"); opens != closes {
		return true
	}
	return false
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
		return
	}
	text := msg.Message.GetConversation()
	if text == "" {
		text = msg.Message.GetExtendedTextMessage().GetText()
	}
	if text == "" {
		return
	}
	b.activeM.RLock()
	c := b.active
	b.activeM.RUnlock()
	if c != nil {
		c.put(msg.Info.ID, text)
	}
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
	var parts []struct{ Text string `json:"text"` }
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
