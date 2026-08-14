package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The real speech service, if it is running. These tests refuse to pass
// without it: a voice test that stubs the audio proves nothing about audio.
func speechAt(t *testing.T) *Speech {
	t.Helper()
	at := os.Getenv("VOICE_SPEECH")
	if at == "" {
		at = "http://127.0.0.1:8123"
	}
	resp, err := http.Get(at + "/healthz")
	if err != nil {
		t.Skipf("speech is not running at %s: %v", at, err)
	}
	resp.Body.Close()
	return NewSpeech(at)
}

// saysBack is a language model that answers with a fixed line, streamed the
// way a real one streams. Only the model is stood in for; the transcriber and
// the synthesiser in these tests are the real ones.
func saysBack(reply string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, word := range strings.SplitAfter(reply, " ") {
			frame, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{{"delta": map[string]string{"content": word}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// heard collects what the agent said back: the audio deltas, decoded and
// concatenated into one run of PCM.
type heard struct {
	pcm  []byte
	text string
	// The number a conversation is judged on: from the moment the speaker
	// stops to the moment sound comes back. Measuring from the START of the
	// question instead would flatter the result by however long the question
	// took to say, which is not latency — it is the speaker talking.
	answer time.Duration
	// From the first byte of audio sent, for comparison.
	first  time.Duration
	stop   time.Time
	events []string
}

// converse opens a socket, sends the audio, and gathers the reply until the
// agent finishes or the wait runs out.
func converse(t *testing.T, v *Voice, who Who, send []byte, wait time.Duration) heard {
	t.Helper()
	srv := httptest.NewServer(handler(v))
	defer srv.Close()

	ticket := v.Desk.Issue(who)
	at, _ := url.Parse(srv.URL)
	at.Scheme = "ws"
	at.Path = "/v1/voice"
	at.RawQuery = "ticket=" + ticket

	ctx, done := context.WithTimeout(context.Background(), wait+20*time.Second)
	defer done()
	conn, _, err := websocket.Dial(ctx, at.String(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(most)

	// The chunk the streaming transcriber is built around: 250 ms.
	const chunk = Heard * 2 / 4
	go func() {
		for at := 0; at < len(send); at += chunk {
			end := min(at+chunk, len(send))
			msg, _ := json.Marshal(map[string]string{
				"type":  "input_audio_buffer.append",
				"audio": base64.StdEncoding.EncodeToString(send[at:end]),
			})
			if conn.Write(ctx, websocket.MessageText, msg) != nil {
				return
			}
			// Paced like a microphone, so the transcriber overlaps the speech
			// exactly as it would in a real conversation.
			time.Sleep(250 * time.Millisecond)
		}
	}()

	var got heard
	start := time.Now()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		rctx, rdone := context.WithDeadline(ctx, deadline)
		_, msg, err := conn.Read(rctx)
		rdone()
		if err != nil {
			break
		}
		var head struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &head) != nil {
			continue
		}
		label := head.Type
		if head.Type == "error" && head.Error.Message != "" {
			label = "error: " + head.Error.Message
		}
		got.events = append(got.events, label)
		switch head.Type {
		case "input_audio_buffer.speech_stopped":
			got.stop = time.Now()
		case "response.audio.delta":
			pcm, err := base64.StdEncoding.DecodeString(head.Delta)
			if err != nil {
				t.Fatalf("audio delta is not base64: %v", err)
			}
			if got.first == 0 {
				got.first = time.Since(start)
				if !got.stop.IsZero() {
					got.answer = time.Since(got.stop)
				}
			}
			got.pcm = append(got.pcm, pcm...)
		case "response.audio_transcript.delta":
			got.text += head.Delta
		case "response.done":
			return got
		}
	}
	return got
}

func handler(v *Voice) http.Handler {
	mux := http.NewServeMux()
	v.Routes(mux)
	return mux
}

func daemon(t *testing.T, sp *Speech, chat string, reply string) *Voice {
	t.Helper()
	return &Voice{
		Desk:   NewDesk(),
		Room:   NewFloorspace(Capacity, Share),
		Speech: sp,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Model:  "kokoro",
		Voice:  "af_heart",
		Think: func(w Who) (Mind, Turn) {
			return &Pipe{
				Speech: sp, STT: "whisper",
				Chat: &Chat{URL: chat, Model: "stand-in", HTTP: &http.Client{}, Token: w.Token, Org: w.Org},
			}, &Hush{QuietMS: 500}
		},
	}
}

var someone = Who{User: "u_test", Org: "hanzo", Name: "test", Token: "test-bearer"}

// TestAudioGoesBothWays is the whole claim: real speech in, real speech out.
//
// The question is spoken by the synthesiser, sent as audio, transcribed by the
// real transcriber, answered by a stand-in model, synthesised again, and the
// reply audio is transcribed a second time to check that what came back is
// speech carrying the right words — not silence, and not nothing.
func TestAudioGoesBothWays(t *testing.T) {
	sp := speechAt(t)
	const reply = "The meeting moved to Thursday at two o'clock."
	chat := saysBack(reply)
	defer chat.Close()

	ctx := context.Background()
	question, err := sp.Say(ctx, "kokoro", "af_heart", "When is the meeting?")
	if err != nil {
		t.Fatalf("could not synthesise the question: %v", err)
	}
	question = downsample(question)
	question = append(question, make([]byte, Heard*2)...) // a second of quiet ends the turn

	got := converse(t, daemon(t, sp, chat.URL, reply), someone, question, 60*time.Second)

	if len(got.pcm) == 0 {
		t.Fatalf("no audio came back at all; events seen: %v", got.events)
	}
	spoke := float64(len(got.pcm)) / 2 / Spoken
	if spoke < 0.5 {
		t.Fatalf("audio came back but only %.2fs of it", spoke)
	}
	if quiet(got.pcm) {
		t.Fatalf("audio came back but it is silent (%.2fs of zeros) — "+
			"a silent stream must not pass as a working one", spoke)
	}

	// The reply audio has to say the right thing. Transcribing it closes the
	// loop: nothing short of real audio carrying real words gets here.
	back, err := sp.Transcribe(ctx, "whisper", wav(got.pcm, Spoken))
	if err != nil {
		t.Fatalf("could not transcribe the reply: %v", err)
	}
	if overlap(reply, back) < 0.6 {
		t.Fatalf("reply audio said %q, expected something like %q", back, reply)
	}
	if got.answer == 0 {
		t.Fatalf("the turn never ended: no speech_stopped before the reply; events %v", got.events)
	}
	t.Logf("ANSWER LATENCY %v  (speaker stopped -> first sound)",
		got.answer.Round(time.Millisecond))
	t.Logf("  reply is %.2fs of audio, saying %q", spoke, strings.TrimSpace(back))
	t.Logf("  from first byte sent: %v (includes %.2fs of the question being said)",
		got.first.Round(time.Millisecond), (got.first - got.answer).Seconds())
}

// TestSilenceIsNotAnAnswer is the negative half. Without it the test above
// cannot fail: an implementation that replies to anything would pass it.
func TestSilenceIsNotAnAnswer(t *testing.T) {
	sp := speechAt(t)
	chat := saysBack("This should never be said.")
	defer chat.Close()

	// Three seconds of silence — the same shape as the question, no speech.
	got := converse(t, daemon(t, sp, chat.URL, ""), someone, make([]byte, Heard*2*3), 20*time.Second)

	if len(got.pcm) > 0 {
		back, _ := sp.Transcribe(context.Background(), "whisper", wav(got.pcm, Spoken))
		t.Fatalf("silence drew a spoken reply of %.2fs (%q) — the agent is talking to nobody",
			float64(len(got.pcm))/2/Spoken, strings.TrimSpace(back))
	}
}

// TestTicketIsSpentOnce — a ticket copied from a log or a browser history is
// worthless. This is the property that makes it safe in a URL.
func TestTicketIsSpentOnce(t *testing.T) {
	d := NewDesk()
	name := d.Issue(someone)
	if _, ok := d.Redeem(name); !ok {
		t.Fatal("a fresh ticket was refused")
	}
	if _, ok := d.Redeem(name); ok {
		t.Fatal("the same ticket was accepted twice")
	}
	if _, ok := d.Redeem("not-a-ticket"); ok {
		t.Fatal("an invented ticket was accepted")
	}
}

// TestSocketNeedsATicket — no ticket, no upgrade, and the refusal happens
// before the socket exists rather than after.
func TestSocketNeedsATicket(t *testing.T) {
	v := &Voice{Desk: NewDesk(), Room: NewFloorspace(Capacity, Share),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(handler(v))
	defer srv.Close()

	for _, q := range []string{"", "?ticket=", "?ticket=guessed"} {
		resp, err := http.Get(srv.URL + "/v1/voice" + q)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%q upgraded or misrefused: got %d, want 401", q, resp.StatusCode)
		}
	}
}

// TestRoomFillsUp — the fifth conversation is refused rather than queued, and
// one tenant cannot take every slot.
func TestRoomFillsUp(t *testing.T) {
	f := NewFloorspace(4, 2)
	if _, err := f.Enter("a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := f.Enter("a"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := f.Enter("a"); err == nil {
		t.Fatal("one org took more than its share")
	}
	leave1, _ := f.Enter("b")
	if _, err := f.Enter("b"); err != nil {
		t.Fatalf("other org: %v", err)
	}
	if _, err := f.Enter("c"); err == nil {
		t.Fatal("a fifth conversation was admitted")
	}
	leave1()
	if _, err := f.Enter("c"); err != nil {
		t.Fatalf("a slot did not come back: %v", err)
	}
	if f.Busy() != 4 {
		t.Fatalf("busy says %d, want 4", f.Busy())
	}
}

// --- helpers ---------------------------------------------------------------

// downsample takes kokoro's 24 kHz to the transcriber's 16 kHz by dropping one
// sample in three. Crude, and adequate: it is the test's microphone.
func downsample(pcm []byte) []byte {
	out := make([]byte, 0, len(pcm)*2/3)
	for i := 0; i+1 < len(pcm); i += 2 {
		if (i/2)%3 == 2 {
			continue
		}
		out = append(out, pcm[i], pcm[i+1])
	}
	return out
}

func quiet(pcm []byte) bool { return loudest(pcm) < 300 }

func overlap(want, got string) float64 {
	keep := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.Fields(strings.ToLower(s)) {
			out[strings.Trim(w, ".,!?'\"")] = true
		}
		return out
	}
	a, b := keep(want), keep(got)
	if len(a) == 0 {
		return 0
	}
	var hit int
	for w := range a {
		if b[w] {
			hit++
		}
	}
	return float64(hit) / float64(len(a))
}

// wav wraps raw PCM16 so the batch transcriber will read it.
func wav(pcm []byte, rate int) []byte {
	head := make([]byte, 44)
	copy(head[0:], "RIFF")
	put32(head[4:], uint32(36+len(pcm)))
	copy(head[8:], "WAVEfmt ")
	put32(head[16:], 16)
	put16(head[20:], 1)
	put16(head[22:], 1)
	put32(head[24:], uint32(rate))
	put32(head[28:], uint32(rate*2))
	put16(head[32:], 2)
	put16(head[34:], 16)
	copy(head[36:], "data")
	put32(head[40:], uint32(len(pcm)))
	return append(head, pcm...)
}

func put16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func put32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}
