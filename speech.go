package voice

import (
	"bytes"
	"context"
	"encoding/binary"
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

// Say turns text into raw PCM16 at Spoken Hz.
//
// wav is asked for rather than pcm even though pcm is what is wanted. Every
// format except wav is produced by piping the wav through an ffmpeg
// subprocess, so asking for "pcm" spawns a process to strip a 44-byte header.
// Asking for wav and dropping those bytes here does the same thing without the
// process, on a path that runs once per sentence.
func (s *Speech) Say(ctx context.Context, model, voice, text string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{
		"model": model, "input": text, "voice": voice, "response_format": "wav",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("speech %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	wav, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return samples(wav)
}

// samples finds the data chunk of a RIFF/WAVE file and returns it.
//
// The header is 44 bytes for what soundfile writes, but a fixed 44 is a
// latent corruption the day a chunk is added ahead of `data` — walking the
// chunks costs nothing and cannot be wrong.
func samples(wav []byte) ([]byte, error) {
	if len(wav) < 12 || !bytes.Equal(wav[0:4], []byte("RIFF")) || !bytes.Equal(wav[8:12], []byte("WAVE")) {
		return nil, fmt.Errorf("not a wav (%d bytes)", len(wav))
	}
	for at := 12; at+8 <= len(wav); {
		id := wav[at : at+4]
		size := int(binary.LittleEndian.Uint32(wav[at+4 : at+8]))
		at += 8
		if bytes.Equal(id, []byte("data")) {
			if at+size > len(wav) {
				size = len(wav) - at // truncated tail: keep what arrived
			}
			return wav[at : at+size], nil
		}
		at += size + size%2 // chunks are word aligned
	}
	return nil, fmt.Errorf("wav carried no data chunk")
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
