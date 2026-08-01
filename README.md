# wa-metaai

OpenAI-compatible endpoint backed by Meta AI, reached over WhatsApp. Lets opencode (or any
OpenAI-compatible client) use Meta AI as a model.

```
opencode --> POST localhost:8788/v1/chat/completions --> whatsmeow --> WhatsApp
                                                     <-- Meta AI (867051314767696@bot)
```

## Why whatsmeow and not Baileys

Meta AI is not a phone-number contact. It is a **bot JID**: `867051314767696@bot`. Delivery to a bot
requires two things on the outgoing stanza:

- a `<bot>` node appended to the stanza content
- an HKDF-derived `BotMessageSecret` (`applyBotMessageHKDF` over the message secret)

Baileys implements neither — its entire send path references bot JIDs once, only to skip issuing a
TC token. The observable symptom is precise and misleading: WhatsApp **server-acks** the message
(`status: 2`) and then never delivers it, so it sits at one grey tick forever and Meta AI never
replies. A control send to a human JID on the same code path went PENDING → 2 → 3 → 4 (read),
proving the transport was fine and only bot support was missing.

whatsmeow has it: `types.NewMetaAIJID` is exactly `867051314767696@bot`, `send.go` sets `isBotMode`
when the target `IsBot()`, derives the bot secret, and attaches the `<bot>` node.

Baileys' `META_AI_JID = 13135550002@c.us` is legacy and unroutable — sending to it produces
`USync fetch yielded no results for pending PNs`, WhatsApp's way of saying that number isn't a user.

## Setup

The account itself must live in a real WhatsApp mobile app (an Android emulator works); this service
attaches as a **linked device** and cannot hold the registration on its own.

```bash
CGO_ENABLED=1 go build -o wametaai .
WA_PHONE=<digits, country code first> ./wametaai
```

First run prints a pairing code — enter it under **WhatsApp > Linked devices > Link with phone
number**. The session persists in `wametaai.db`, so later runs need no code.

## opencode config

`~/.config/opencode/opencode.json`:

```json
{
  "provider": {
    "whatsapp": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "WhatsApp (Meta AI)",
      "options": { "baseURL": "http://localhost:8788/v1", "apiKey": "unused" },
      "models": {
        "meta-ai": { "name": "Meta AI (WhatsApp)", "tool_call": false, "limit": { "context": 8000, "output": 4000 } }
      }
    }
  }
}
```

Then `opencode run --model whatsapp/meta-ai "..."`.

## Limits

| | |
|---|---|
| tool calling | **none** — `tool_call: false` is load-bearing. Meta AI returns prose, so opencode's read/edit/bash agent loop cannot run on this model. Chat and questions only. |
| streaming | reply is buffered then emitted as one chunk. Meta AI answers as *many separate messages* (~10 message IDs per answer), not edits to one, so they are joined in arrival order and considered complete after `WA_QUIET_MS` of silence. |
| system prompt | folded into a flattened transcript; there is no separate system role |
| conversation state | one WhatsApp thread with its own memory, so history is resent each turn and Meta AI's own recall can bleed across requests |
| concurrency | serialized — a single thread cannot serve parallel requests |
| token usage | estimated at ~4 chars/token; WhatsApp reports none |
| latency | ~7s for a short answer |

## Env

| var | default | |
|---|---|---|
| `WA_PORT` | `8788` | HTTP port |
| `WA_PHONE` | — | required only for first-time pairing |
| `WA_QUIET_MS` | `4000` | reply considered complete after this much silence |
| `WA_TIMEOUT_MS` | `120000` | hard cap per request |

## Account durability

Registered on a rented TextVerified number, so the account lives only as long as that rental — a
released number can be re-rented and re-registered by someone else, which would evict this account.
WhatsApp also expires linked devices when the primary stays offline, so keep the emulator reachable.
Unofficial-client use is against WhatsApp's terms and carries a ban risk for the number.
