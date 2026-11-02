package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Rates the two models actually work at. Neither is a choice: whisper resamples
// everything to 16 kHz internally, and kokoro emits 24 kHz. Naming them here
// keeps the arithmetic in one place instead of scattered magic numbers.
const (
	Heard  = 16000 // what the transcriber is fed
	Spoken = 24000 // what the synthesiser produces
)

// Speech is the batch speech service — the models, and nothing else. It holds
// no conversation: this package does.
type Speech struct {
	URL  string
	HTTP *http.Client
}

func NewSpeech(url string) *Speech {
	// No overall client timeout: a synthesis is bounded by its context, and a
	// deadline that fires mid-utterance would truncate speech rather than fail.
	return &Speech{URL: strings.TrimRight(url, "/"), HTTP: &http.Client{}}
}

// Say speaks text and hands the audio over as it arrives: raw PCM16 at Spoken
// Hz, in pieces that hold whole samples. The speech service streams pcm a
// sentence at a time, so the first piece comes back while the rest of the text
// is still being synthesised — the wait to first sound is one sentence, not the
// reply. language is the text's ISO-639-1 code; "" lets the service read it off
// the script, which is how Arabic finds the voice that speaks it.
//
// each is called with every piece; an error from it stops the read and is
// returned. A piece is only valid for the duration of the call.
func (s *Speech) Say(ctx context.Context, model, voice, language, text string, each func(pcm []byte) error) error {
	body, _ := json.Marshal(map[string]string{
		"model": model, "input": text, "voice": voice, "language": language, "response_format": "pcm",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("speech %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	buf := make([]byte, Spoken/5*2) // 200 ms
	var carry []byte
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			piece := append(carry, buf[:n]...)
			whole := len(piece) - len(piece)%2
			if whole > 0 {
				if e := each(piece[:whole]); e != nil {
					return e
				}
			}
			carry = append([]byte(nil), piece[whole:]...)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Ear is an open transcript on the speech service: audio goes in as it is
// spoken, text comes back as it is decoded.
type Ear struct {
	s     *Speech
	id    string
	model string
}

// Listen opens a transcript. The service refuses anything but mono PCM16 at
// its own rate by name, so the format is stated rather than negotiated.
func (s *Speech) Listen(ctx context.Context, model string) (*Ear, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "format": "pcm16", "rate": Heard, "channels": 1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/audio/transcript", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("transcript open %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &Ear{s: s, id: out.ID, model: model}, nil
}

// Said is what the transcriber has made of the audio so far. Text is settled;
// Pending is the moving tail that may still change. Seconds is what this push
// consumed, which is what gets billed.
type Said struct {
	Text    string  `json:"text"`
	Pending string  `json:"pending"`
	Seconds float64 `json:"duration"`
	// Language is what the speaker is speaking, ISO-639-1, once the transcriber
	// was told or has identified it; "" before. A reply is spoken in it.
	Language string `json:"language"`
}

// All is everything heard, settled and not. A turn ends on the whole of it.
func (s Said) All() string { return strings.TrimSpace(s.Text + " " + s.Pending) }

// Push hands over a chunk of audio and returns what is understood so far.
func (e *Ear) Push(ctx context.Context, pcm []byte) (Said, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.s.URL+"/v1/audio/transcript/"+e.id, bytes.NewReader(pcm))
	if err != nil {
		return Said{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := e.s.HTTP.Do(req)
	if err != nil {
		return Said{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Said{}, fmt.Errorf("transcript push %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var said Said
	if err := json.NewDecoder(resp.Body).Decode(&said); err != nil {
		return Said{}, err
	}
	return said, nil
}

func (e *Ear) Close() error {
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, e.s.URL+"/v1/audio/transcript/"+e.id, nil)
	if err != nil {
		return err
	}
	resp, err := e.s.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Transcribe is the batch route, for audio that is already whole — an
// utterance handed over after the fact rather than as it is spoken.
func (s *Speech) Transcribe(ctx context.Context, model string, wav []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("model", model)
	part, err := w.CreateFormFile("file", "a.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("transcribe %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out struct{ Text string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Text, nil
}
