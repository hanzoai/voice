package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Say hands audio over as the speech service streams it, and asks for raw pcm
// in the language it was given.
func TestSayStreamsPiecesAsTheyArrive(t *testing.T) {
	var asked map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&asked)
		w.Header().Set("Content-Type", "audio/pcm")
		w.Write(make([]byte, 4801)) // an odd byte: half a sample, finished by the next write
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		w.Write(make([]byte, 4801))
	}))
	defer srv.Close()

	began := time.Now()
	var at []time.Duration
	var total int
	err := NewSpeech(srv.URL).Say(context.Background(), "kokoro", "M1", "ar", "مرحباً. كيف حالك؟", func(pcm []byte) error {
		if len(pcm)%2 != 0 {
			t.Errorf("a piece of %d bytes splits a sample", len(pcm))
		}
		at = append(at, time.Since(began))
		total += len(pcm)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if asked["language"] != "ar" || asked["response_format"] != "pcm" || asked["voice"] != "M1" {
		t.Fatalf("asked %v", asked)
	}
	if len(at) < 2 || at[0] > 250*time.Millisecond || total != 9602 {
		t.Fatalf("pieces at %v, %d bytes: the first must arrive before the second is written", at, total)
	}
}

type heardIn struct{ thinking }

func (heardIn) Language() string { return "ar" }

// session.update sets the voice and the language; without one, a reply is
// spoken in the language the speaker was heard in.
func TestSessionUpdateChoosesVoiceAndLanguage(t *testing.T) {
	ev, err := Read([]byte(`{"type":"session.update","session":{"voice":"F3","input_audio_transcription":{"model":"whisper","language":"ar"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Voice: "af_heart", Mind: &thinking{}}
	if err := s.take(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if v, l := s.tongue(); v != "F3" || l != "ar" {
		t.Fatalf("voice %q language %q after the update", v, l)
	}
	s = &Session{Voice: "af_heart", Mind: &heardIn{}}
	if _, l := s.tongue(); l != "ar" {
		t.Fatalf("a speaker heard in Arabic is answered in %q", l)
	}
}

// The Pipe keeps the language the transcriber reports, across turns.
func TestPipeRemembersTheLanguageItHeard(t *testing.T) {
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v1/audio/transcript" {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"atr_1"}`))
			return
		}
		w.Write([]byte(`{"text":"مرحبا","pending":"","duration":0.5,"language":"ar"}`))
	}))
	defer srv.Close()
	p := &Pipe{Speech: NewSpeech(srv.URL), STT: "parakeet"}
	p.Hear(context.Background(), make([]byte, 8000))
	deadline := time.Now().Add(3 * time.Second)
	for p.Language() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Language() != "ar" {
		t.Fatalf("language %q", p.Language())
	}
}
