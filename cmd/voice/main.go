// Command voice serves a spoken conversation over one socket.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hanzoai/voice"
)

func main() {
	var (
		addr    = flag.String("addr", ":8140", "address to serve on")
		speech  = flag.String("speech", env("VOICE_SPEECH", "http://speech.hanzo.svc"), "speech service")
		ai      = flag.String("ai", env("VOICE_AI", "https://api.hanzo.ai"), "language model service")
		issuer  = flag.String("iam", env("VOICE_IAM", "https://hanzo.id"), "IAM issuer")
		model   = flag.String("model", env("VOICE_MODEL", "zen-omni"), "language model")
		stt     = flag.String("stt", env("VOICE_STT", "whisper"), "transcriber")
		tts     = flag.String("tts", env("VOICE_TTS", "kokoro"), "synthesiser")
		who     = flag.String("voice", env("VOICE_VOICE", "af_heart"), "synthesised voice")
		prompt  = flag.String("prompt", env("VOICE_PROMPT", "You are a voice assistant. Answer in one or two short sentences. Never use lists or markdown: everything you write will be spoken aloud."), "system prompt")
		origins = flag.String("origins", env("VOICE_ORIGINS", ""), "comma separated origins allowed to open a socket")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	v := &voice.Voice{
		Gate:   voice.NewGate(*issuer+"/v1/iam/.well-known/jwks", []string{*issuer}, nil),
		Desk:   voice.NewDesk(),
		Room:   voice.NewFloorspace(voice.Capacity, voice.Share),
		Speech: voice.NewSpeech(*speech),
		Log:    log,
		Meter:  ledger{log},
		Model:  *tts,
		Voice:  *who,
		Think: func(w voice.Who) (voice.Mind, voice.Turn) {
			return &voice.Pipe{
				Speech: voice.NewSpeech(*speech),
				STT:    *stt,
				Prompt: *prompt,
				Chat: &voice.Chat{
					URL: *ai, Model: *model, HTTP: &http.Client{},
					// The caller's own bearer and org: audio is billed to
					// whoever is speaking, never to a service account.
					Token: w.Token, Org: w.Org,
				},
			}, &voice.Hush{}
		},
	}
	if *origins != "" {
		v.Origins = strings.Split(*origins, ",")
	}

	mux := http.NewServeMux()
	v.Routes(mux)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Conversations in flight get a moment to finish their sentence.
		bye, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		_ = srv.Shutdown(bye)
	}()

	log.Info("voice listening", "addr", *addr, "speech", *speech, "ai", *ai,
		"capacity", voice.Capacity, "per_org", voice.Share)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("voice stopped", "why", err)
		os.Exit(1)
	}
}

func env(name, or string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return or
}

// ledger records what was spoken and heard. Seconds are the billable quantity
// and they are attributed to a person, which is the whole point of refusing a
// credential that names no subject.
type ledger struct{ log *slog.Logger }

func (l ledger) Bill(w voice.Who, heard, spoke time.Duration) {
	l.log.Info("audio",
		"org", w.Org, "user", w.User,
		"heard_s", heard.Seconds(), "spoke_s", spoke.Seconds())
}
