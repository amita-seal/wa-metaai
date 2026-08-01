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
	quietTime = envDuration("WA_QUIET_MS", 4000)
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
			return c.value()
		case <-deadline:
			return c.value()
		}
	}
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

type chatReq struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
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

func flatten(r chatReq) string {
	var turns []string
	for _, m := range r.Messages {
		t := contentText(m.Content)
		if strings.TrimSpace(t) == "" {
			continue
		}
		turns = append(turns, fmt.Sprintf("[%s]\n%s", strings.ToUpper(m.Role), t))
	}
	if len(turns) == 0 {
		return ""
	}
	return strings.Join(turns, "\n\n") + "\n\n[ASSISTANT]"
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
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-" + msgID(), "object": "chat.completion", "created": created, "model": modelID,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": reply}}},
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
	chunk(map[string]any{"content": reply}, nil)
	chunk(map[string]any{}, "stop")
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
