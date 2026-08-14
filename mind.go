package voice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Pipe is the mind that transcribes and then thinks.
//
// The transcriber runs while the speaker is still talking, so by the time the
// floor is yielded the words are already there and only the model's own
// latency is left. That overlap is the reason this composition beats handing a
// whole utterance to an audio model after the fact, even though the audio
// model is one inference instead of two: the transcription was free, it
// happened during speech.
//
// What it costs is tone. "Fine." and "Fine?" reach the model as the same word
// once prosody is thrown away at the transcript boundary. Omni carries that;
// this does not.
type Pipe struct {
	Speech *Speech
	Chat   *Chat
	STT    string
	Prompt string

	mu       sync.Mutex
	ear      *Ear
	said     strings.Builder
	frames   [][]byte
	turns    []line
	waiting  []byte // audio set down but not yet carried to the transcriber
	carrying bool   // a worker is carrying it
}

type line struct{ Role, Text string }

// Hear takes audio and returns immediately.
//
// It has to. A microphone does not pause while a decoder catches up, and the
// caller of this is the loop that also watches for the end of the turn. So the
// audio is set down here and a single worker carries it to the transcriber.
//
// When the transcriber falls behind, the waiting audio coalesces: the worker
// takes everything set down since it last looked and sends it as one push.
// Nothing is dropped — dropped audio is a hole in the middle of a sentence,
// and the transcript reads as if the speaker said something they did not — and
// the request rate falls to whatever the transcriber can actually sustain,
// which is the only rate that was ever going to work.
func (p *Pipe) Hear(ctx context.Context, pcm []byte) {
	p.mu.Lock()
	p.waiting = append(p.waiting, pcm...)
	if p.carrying {
		p.mu.Unlock()
		return
	}
	p.carrying = true
	p.mu.Unlock()
	go p.carry(context.WithoutCancel(ctx))
}

// carry moves waiting audio to the transcriber until there is none left.
func (p *Pipe) carry(ctx context.Context) {
	for {
		p.mu.Lock()
		batch := p.waiting
		p.waiting = nil
		if len(batch) == 0 {
			p.carrying = false
			p.mu.Unlock()
			return
		}
		if p.ear == nil {
			ear, err := p.Speech.Listen(ctx, p.STT)
			if err != nil {
				p.carrying = false
				p.mu.Unlock()
				return
			}
			p.ear = ear
		}
		ear := p.ear
		p.mu.Unlock()

		said, err := ear.Push(ctx, batch)
		if err != nil {
			p.mu.Lock()
			p.carrying = false
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		p.said.Reset()
		p.said.WriteString(said.All())
		p.mu.Unlock()
	}
}

// settled waits for the audio already handed over to come back as words.
//
// Waiting for the bytes to be carried is not enough. A transcriber given a
// quarter second of speech at a time has usually returned nothing at all by
// the moment the speaker stops — the words are still inside a decode. Asking
// for the transcript then finds it empty and answers "nothing was said" to
// someone who just spoke a whole sentence.
//
// So the wait is for text, not for bytes, and it is bounded: silence never
// produces words, and a turn that ends without any is one there is nothing to
// answer. The bound is what keeps that case fast instead of hanging.
func (p *Pipe) settled(ctx context.Context) {
	const patience = 4 * time.Second
	give := time.Now().Add(patience)
	for time.Now().Before(give) {
		p.mu.Lock()
		idle := !p.carrying && len(p.waiting) == 0
		words := p.said.Len() > 0
		p.mu.Unlock()
		if idle && words {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (p *Pipe) See(jpeg []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Only the newest frame is kept. A model call per frame is ruinous and
	// pointless: what matters is what is on screen when the question is asked,
	// not the thirty frames of it that went past while it was being asked.
	p.frames = [][]byte{jpeg}
}

func (p *Pipe) Reply(ctx context.Context) (<-chan string, error) {
	p.settled(ctx)
	p.mu.Lock()
	heard := strings.TrimSpace(p.said.String())
	frames := p.frames
	p.frames = nil
	p.said.Reset()
	if p.ear != nil {
		_ = p.ear.Close()
		p.ear = nil
	}
	if heard == "" {
		p.mu.Unlock()
		return nil, fmt.Errorf("nothing was said")
	}
	p.turns = append(p.turns, line{"user", heard})
	turns := append([]line(nil), p.turns...)
	p.mu.Unlock()

	out, err := p.Chat.Stream(ctx, p.Prompt, turns, frames)
	if err != nil {
		return nil, err
	}
	// Remember what was answered, so the next turn has the conversation.
	kept := make(chan string)
	go func() {
		defer close(kept)
		var whole strings.Builder
		for piece := range out {
			whole.WriteString(piece)
			kept <- piece
		}
		p.mu.Lock()
		p.turns = append(p.turns, line{"assistant", whole.String()})
		p.mu.Unlock()
	}()
	return kept, nil
}

// Chat is the language model, reached the way every other caller reaches it.
type Chat struct {
	URL   string
	Model string
	HTTP  *http.Client
	// Token is the CALLER's bearer, not this service's. Audio is expensive and
	// it is billed to whoever is speaking, which requires their credential on
	// the request rather than a service key that pools every tenant onto one
	// account.
	Token string
	Org   string
}

func (c *Chat) Stream(ctx context.Context, prompt string, turns []line, frames [][]byte) (<-chan string, error) {
	msgs := []map[string]any{}
	if prompt != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": prompt})
	}
	for i, t := range turns {
		last := i == len(turns)-1
		if last && t.Role == "user" && len(frames) > 0 {
			parts := []map[string]any{{"type": "text", "text": t.Text}}
			for _, f := range frames {
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]string{
						"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(f),
					},
				})
			}
			msgs = append(msgs, map[string]any{"role": t.Role, "content": parts})
			continue
		}
		msgs = append(msgs, map[string]any{"role": t.Role, "content": t.Text})
	}
	body, _ := json.Marshal(map[string]any{
		"model": c.Model, "messages": msgs, "stream": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if c.Org != "" {
		req.Header.Set("X-Org-Id", c.Org)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("chat %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}

	out := make(chan string)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scan := bufio.NewScanner(resp.Body)
		scan.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scan.Scan() {
			data, ok := strings.CutPrefix(scan.Text(), "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var frame struct {
				Choices []struct {
					Delta struct{ Content string }
				}
			}
			if json.Unmarshal([]byte(data), &frame) != nil || len(frame.Choices) == 0 {
				continue
			}
			if piece := frame.Choices[0].Delta.Content; piece != "" {
				select {
				case out <- piece:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Hush is the turn detector of last resort: it ends a turn after a stretch of
// quiet and nothing else.
//
// It is honest about what it is. Silence is a bad proxy for the end of a
// thought — people pause mid-sentence to think and do not pause at all before
// a trailing "right?" — so this will cut people off and it will wait through
// gaps it should have taken. It is here so the surface works end to end while
// the detector that reads the waveform for intent is built against the Turn
// interface, and it is the thing that gets replaced, not extended.
type Hush struct {
	// Quiet is how much silence ends a turn.
	QuietMS int
	// Loud is the amplitude below which a sample counts as silence.
	Loud int16

	quiet int
	spoke bool
}

func (h *Hush) Hear(pcm []byte) Floor {
	if h.QuietMS == 0 {
		h.QuietMS = 700
	}
	if h.Loud == 0 {
		h.Loud = 700
	}
	ms := len(pcm) * 1000 / (Heard * 2)
	if loudest(pcm) > h.Loud {
		h.spoke = true
		h.quiet = 0
		return Held
	}
	if !h.spoke {
		return Quiet
	}
	if h.quiet += ms; h.quiet >= h.QuietMS {
		h.spoke = false
		h.quiet = 0
		return Yielded
	}
	return Held
}

func (h *Hush) Reset() { h.spoke, h.quiet = false, 0 }

func loudest(pcm []byte) int16 {
	var top int16
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		if v < 0 {
			v = -v
		}
		if v > top {
			top = v
		}
	}
	return top
}
