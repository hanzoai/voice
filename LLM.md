# voice

A spoken conversation over one socket. Audio in, audio out, billed to whoever
spoke. `hanzoai/speech` holds the models; this holds the conversation.

## The surface

    POST /v1/voice/session   Authorization: Bearer <IAM>  [X-Org-Id: <org>]
      -> { ticket, expires_in, rate: 24000, format: "pcm16", channels: 1 }

    GET  /v1/voice?ticket=<ticket>          (WebSocket, subprotocol "realtime")
    GET  /v1/voice/health                   -> { ok, busy, capacity }

Two calls because a browser cannot put a header on a WebSocket. The bearer is
spent on the POST, where a header works; what goes in the URL is a ticket — a
random name for a principal this process has already verified, good for one use
and thirty seconds. It carries no claims and proves nothing on its own, so a
copy taken from a log is worthless. Same pattern the sandbox terminal uses.

## The wire

The OpenAI realtime protocol, so clients written against it work unchanged.
Event types come from `hanzoai/go-openai-realtime`, which is a *client* — it can
marshal what a client sends and unmarshal what a server sends. Serving the
other side needed only the missing direction, which is `Read` in `wire.go`.

Consumed: `input_audio_buffer.append` (base64 PCM16 @ 16 kHz), `.commit`,
`response.create`, `response.cancel`, `conversation.item.truncate`,
`session.update`, and `input_video_buffer.append` — a local extension, because
the protocol carries no picture and a model that can see needs one.

Emitted: `session.created`, `input_audio_buffer.speech_started`/`.speech_stopped`,
`response.created`, `response.audio_transcript.delta`, `response.audio.delta`
(base64 PCM16 @ 24 kHz), `response.done`, `error`.

A reply works on a context a barge-in cancels and writes the socket on the session's,
which nothing cancels: coder/websocket closes the connection when a write's context is
done, so a write on the cancelled one ended the conversation, not the turn. A reply
clears the floor on its way out only while the floor is still its own (`finish`).

## The three seams

`Turn` — audio in, floor out (`Quiet`/`Held`/`Yielded`). `Hush` ships with it and
ends a turn on silence alone, which is wrong in both directions: people pause
mid-sentence, and do not pause before a trailing "right?". It is the thing to
replace, not extend. A waveform model belongs here.

`Mind` — hears an utterance, answers in text. `Pipe` transcribes then thinks.
An omni implementation would hear the audio itself and keep the tone the
transcript throws away. The surface cannot tell them apart, which is the point:
one way in, whatever is behind it.

`Meter` — seconds heard and spoken, per principal.

## What is known, and how

Measured on CPU, 32 cores under load ~12, real models both ends:

- **Answer latency 1.15–1.55 s** (median ~1.34 s), speaker stops to first sound.
  Not yet conversational. Of it, 500 ms is `Hush` waiting out silence — dead
  time a semantic detector removes outright.
- **Sentence chunking is 5.0× to first sound**: 2267 ms to synthesise a whole
  reply against 452 ms for its first sentence. After the first, synthesis runs
  while the previous clip plays and is free — kokoro runs 3.5–4.7× faster than
  speech. This is why `Sentences` exists.
- **Speech is asked for `pcm`, and read as it streams.** hanzoai/speech (Go,
  0.1.17+) writes pcm in Go and streams it a sentence at a time, so `Say` hands
  each piece to the socket as it arrives (`each`), whole samples only, and a
  barge-in stops the read mid-sentence. The wav-and-strip-the-header path was
  for the Python service, whose pcm went through ffmpeg.

## Languages

A reply is spoken in the language the speaker used. The transcript reports it
(`Said.Language`: named, or identified by the speech service after 2 s of
speech), the `Pipe` keeps it across turns, and `Say` passes it to the speech
service, which picks the voice that speaks it — Arabic goes to Supertonic,
since no Kokoro voice reads Arabic script. A client fixes both with
`session.update`: `session.voice` (a kokoro or OpenAI name, or supertonic's
`F1`–`F5`, `M1`–`M5`) and `session.input_audio_transcription.language`
(ISO-639-1); the library's `ClientSession` predates that field, so
`SessionUpdate` (wire.go) reads the event itself. The default prompt
(`Prompt`) answers in the speaker's language, and in Arabic in their dialect.

## Capacity

Four conversations, two per tenant. Four is measured, not chosen: the
transcriber runs four decode workers and past four the backlog grows without
bound. `speech` itself enforces nothing — it has no notion of a caller — so the
doorway is here. It refuses rather than queues: a session that waits has
already failed, because the speaker is holding a live microphone.

## Traps

- **Decide the floor before handing audio on, never after.** Turn detection
  behind a network call means the turn is noticed however late the transcriber
  is running, and at four conversations it will be running late.
- **Wait for words, not for bytes.** A transcriber fed a quarter second at a
  time has usually returned nothing when the speaker stops. Asking then finds
  an empty transcript and answers "nothing was said" to someone who just spoke.
- **`owner` is not the tenant.** It names the application a token was minted
  through. The org is `Claims.Home()`; `EffectiveOrg` honours `X-Org-Id` only
  when the signed membership set contains it.
- A credential with no subject is refused. Audio seconds have to land on a
  person for caller-pays to mean anything.
- Origins must be set in production. A WebSocket is exempt from the same-origin
  policy, so that list is all that stops any page opening a microphone session
  as a logged-in user.

## Testing

`talk_test.go` speaks a question with the real synthesiser, sends it as audio,
and transcribes the reply audio back. It is mutation-proven: replacing the
reply with zeros and removing it entirely both fail, with different messages —
a silent stream and an absent stream are distinguishable. Needs `speech`
running; `VOICE_SPEECH` points at it.
