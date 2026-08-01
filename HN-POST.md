# Using Meta AI inside WhatsApp as the LLM behind a coding agent

I wanted to know whether I could point opencode, an open source coding agent, at Meta AI: the
assistant Meta shipped inside WhatsApp. Meta AI has no API, so the only way in is WhatsApp itself.

It works, including tool use:

```
$ opencode run --model whatsapp/meta-ai "read notes.txt and tell me the secret word"
> build · meta-ai
→ Read notes.txt
pomegranate
```

That is a real agent loop. Meta AI decides to call the read tool, opencode executes it, the result
goes back, and Meta AI answers from the file contents.

The setup is a small Go service that speaks the OpenAI chat completions API on localhost and relays
each request as a WhatsApp message. opencode itself needed no changes at all, just a provider block
pointing at the local baseURL.

## The interesting part: Baileys cannot do this

Meta AI is not a phone number contact. It is a bot JID: `867051314767696@bot`.

Sending to it with Baileys produces a convincing illusion of success. WhatsApp server acks the
message (status 2) and then never delivers it. One grey tick forever. No error, no decryption
failure, no reply. What isolated it was a control test: the same code sending to a human JID went
PENDING, then server ack, then delivered, then read. So the transport was fine and only bot support
was missing.

Reading whatsmeow's `send.go` explained why. Bot delivery needs two things on the outgoing stanza:

* a `<bot>` node appended to the stanza content
* an HKDF derived `BotMessageSecret`

Baileys computes neither. Its entire send path references bot JIDs exactly once, and only to skip
issuing a TC token. whatsmeow implements both, and ships the constant
`NewMetaAIJID = 867051314767696@bot`, which matched byte for byte the JID I had already sniffed off
the wire. Worth noting: web searches confidently told me the JID was `11111111111@bot`. It is not.

Also useful for anyone debugging this: sending to Baileys' legacy `13135550002@c.us` logs
`USync fetch yielded no results for pending PNs`, which is WhatsApp's way of saying that number is
not a user.

## Tool calling, since Meta AI has none

Meta AI does not do function calling, so the shim supplies it. Tool schemas are rendered into the
prompt with an instruction to reply with a single fenced `{"tool": ..., "args": {...}}` object, and
the reply is parsed back into a real OpenAI `tool_calls` message with `finish_reason: "tool_calls"`.
Earlier turns are replayed as `[ASSISTANT ran tool]` and `[TOOL RESULT]` so it does not repeat calls.

The parser has to tolerate "Sure! Here you go:" wrapped around the JSON, because a consumer chat
assistant does that constantly.

## Numbers

* 6 to 7 seconds per round trip
* opencode's full system prompt plus its tool schemas came to roughly 37,000 characters per request,
  and Meta AI handled it without complaint. I expected this to be the thing that broke.
* Meta AI answers as about 10 separate WhatsApp messages, not edits to a single one, so they are
  joined in arrival order and treated as complete after a quiet period.

## Caveats, because this is a toy

* Unofficial WhatsApp clients violate the terms of service and put the number at risk of a ban. I
  registered a rented number inside an Android emulator rather than using my own.
* A WhatsApp text message caps out around 65,000 characters. A trivial single file task already used
  37,000, so anything with real context will hit that wall.
* Tool calling is prompted rather than native, so any turn that answers in prose where JSON was
  wanted stalls the loop.
* One WhatsApp thread means requests are serialized, and Meta AI's own conversation memory bleeds
  between them.

## The smaller, more useful takeaway

If you ever want to add a provider to opencode, you probably do not need to modify opencode. Its
model catalog comes from models.dev, but a local OpenAI compatible endpoint plus a `provider` block
in `opencode.json` is enough. The provider abstraction is good enough that the weird part of this
project was WhatsApp, not the agent.
