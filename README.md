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
        "meta-ai": { "name": "Meta AI (WhatsApp)", "tool_call": true, "limit": { "context": 16000, "output": 2000 } }
      }
    }
  }
}
```

Then `opencode run --model whatsapp/meta-ai "..."`.

## Tool calling

Meta AI has no native function calling, so the shim supplies it. When a request carries `tools`,
`toolPrompt` renders each tool's name, description and JSON Schema into the prompt and asks for a
single fenced object:

```json
{"tool": "<name>", "args": {...}}
```

`parseToolCall` extracts that — tolerating the fence, a language tag, and the surrounding chatter
Meta AI likes to add — and the reply is returned as a real OpenAI `tool_calls` message with
`finish_reason: "tool_calls"`, in both the JSON and SSE paths. Prior turns are replayed as
`[ASSISTANT ran tool]` and `[TOOL RESULT]` so it can see what already ran.

Verified working through opencode's real agent loop:

```
$ opencode run --model whatsapp/meta-ai "Read the file notes.txt and tell me the secret word."
> build · meta-ai
→ Read notes.txt
pomegranate
```

Observed: opencode's full system prompt plus its tool schemas came to ~37,000 characters (~9k
tokens) per request, and Meta AI handled it without complaint, correctly resolving a relative path
to an absolute one. Two round trips, ~6s each.

**The ceiling, and who enforces it.** A single WhatsApp text message caps out around 65k characters,
roughly 16k tokens, and a trivial one-file task already uses 37k characters. Past the cap Meta AI
replies "There was a problem generating a response", which opencode surfaces as a generation error.
An 81,953-character prompt built from one `webfetch` of a JSON payload is what first exposed this.

Two layers keep it in check, and both matter:

- **opencode's auto-compaction**, driven by the `limit` you declare in the provider config. It
  compares the tokens this shim reports against `limit.context` minus the output reserve, so the
  declared numbers must reflect WhatsApp's real ceiling: `context: 16000, output: 2000`. Understating
  them (the original `8000`/`4000`) leaves ~4k usable tokens while normal traffic is ~10k, so
  compaction fires on the wrong boundary. Note it is *reactive* — it acts on the previous response's
  usage, so one turn that balloons from a single large tool result can still overshoot.
- **This shim's own trim**, as the backstop for exactly that case: tool results are clipped
  head-and-tail at `WA_MAX_TOOL_RESULT`, and if the prompt still exceeds `WA_MAX_CHARS` the oldest
  middle turns are dropped with a note, keeping the tool definitions and the newest turns.

The trim hides the ceiling rather than removing it: on a genuinely large session, earlier turns are
silently dropped, so the model can lose detail it was told before.

## Measured behaviour

Five simple agentic tasks, run end to end through opencode. A "turn" is one request to this shim,
which is one WhatsApp round trip. Two consecutive clean runs on the same build:

| task | seconds | turns |
|---|---|---|
| create a file with given contents | 37-56 | 4 |
| read a seeded file and report a value | 45-72 | 3 |
| write a script, run it, report output | 109-111 | 5-7 |
| edit a value in an existing JSON file | 65-74 | 4-5 |
| count files matching a glob | 72-73 | 4 |

About six minutes and roughly 23 turns for all five. Latency is transport, not thinking: every turn
costs one ~6-7s WhatsApp round trip regardless of how hard the question is.

## How Meta AI fails, and what the shim does about it

Every one of these was found by running real tasks, and every one was first attempted as a prompt
rule that Meta AI then ignored. Prompt rules did not hold; argument rewriting and refusals did.

| failure | fix |
|---|---|
| re-runs a finished command forever (33 turns on a one-file task) | a repeat of a call already in the transcript is refused and a plain-text answer demanded |
| writes into the temp directory opencode advertises | absolute scratch paths are rewritten to bare filenames |
| invents a relative `opencode/` directory from the same advertisement | the scratch path is redacted from tool descriptions, and a leading `opencode/` is stripped (`WA_SCRATCH_DIR` to override) |
| `cd`s out of the project, and the bash session is persistent | a leading `cd ... &&` is removed |
| deletes the file it was just asked to create | `rm` is only relayed when the user asked to delete something |

## Limits

| | |
|---|---|
| tool-call reliability | prompted, not native. Meta AI is a consumer assistant, so it can answer in prose where a tool call was wanted; the loop stalls on that turn rather than recovering. |
| streaming | reply is buffered then emitted as one chunk. Meta AI streams by **editing one message** with the full answer so far; those edits arrive as a `protocolMessage` of type `MESSAGE_EDIT` that whatsmeow only unwraps on its history-sync path, so live edits must be unwrapped by hand or the reply stays stuck at the first fragment. Completion is inferred from `WA_QUIET_MS` of silence plus a heuristic that the text does not end mid-sentence, with `WA_GRACE_MS` as a bounded extra wait. |
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
| `WA_QUIET_MS` | `6000` | reply considered complete after this much silence |
| `WA_GRACE_MS` | `18000` | extra wait when the reply still looks unfinished |
| `WA_TIMEOUT_MS` | `120000` | hard cap per request |

## Account durability

Registered on a rented TextVerified number, so the account lives only as long as that rental — a
released number can be re-rented and re-registered by someone else, which would evict this account.
WhatsApp also expires linked devices when the primary stays offline, so keep the emulator reachable.
Unofficial-client use is against WhatsApp's terms and carries a ban risk for the number.
