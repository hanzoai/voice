package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	rt "github.com/hanzoai/go-openai-realtime"
)

// Floor is who holds the conversation. A voice agent is a protocol for taking
// turns, and every hard problem in it is a question about this value.
type Floor int

const (
	Quiet  Floor = iota // nobody is speaking
	Held                // the speaker has the floor and is still using it
	Yielded             // the speaker just finished; a reply is due now
)

// Turn decides when a speaker has finished.
//
// This is the seam, and it is deliberately narrow: audio in, floor out. A
// silence timer satisfies it, and so does a model that hears the difference
// between a pause for breath and the end of a thought. Nothing else in this
// package knows which one it is talking to.
type Turn interface {
	// Hear consumes mono PCM16 at Heard Hz and reports who holds the floor.
	Hear(pcm []byte) Floor
	// Reset forgets the current utterance — called after a reply begins.
	Reset()
}

// Mind hears an utterance and answers in text.
//
// Two implementations exist and the surface cannot tell them apart. One
// transcribes and then thinks; the other hears the audio itself, keeping the
// tone the transcript throws away. Both answer in text, because no model in
// the catalogue emits audio — see Voice.Speak for where sound comes from.
type Mind interface {
	// Hear is fed audio as it arrives, so a mind that transcribes can work
	// while the speaker is still talking instead of after.
	Hear(ctx context.Context, pcm []byte)
	// See offers the most recent video frame, JPEG encoded.
	See(jpeg []byte)
	// Reply closes the utterance and streams the answer as it is composed.
	Reply(ctx context.Context) (<-chan string, error)
}

// Meter records what a session owes. Seconds are the billable quantity in both
// directions and they must land on a person, so the principal is carried with
// them rather than looked up later.
type Meter interface {
	Bill(who Who, heard, spoke time.Duration)
}

// Session is one conversation on one socket.
type Session struct {
	ID     string
	Who    Who
	Sock   Sock
	Speech *Speech
	Mind   Mind
	Turn   Turn
	Meter  Meter
	Log    *slog.Logger

	Model string // synthesiser
	Voice string

	// stop cancels the reply in flight. Barge-in is exactly this being called
	// while audio is still queued.
	mu     sync.Mutex
	stop   context.CancelFunc
	item   atomic.Int64 // conversation item counter, for truncate
	spoken atomic.Int64 // ms of audio sent for the item in flight
	heard  atomic.Int64 // ms of audio taken in
	said   atomic.Int64 // ms of audio sent out
}

// Sock is the socket, narrowed to what a session does with it. Narrowing it
// here is what lets the session be tested without a network.
type Sock interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, msg []byte) error
	Close() error
}

// Run drives one conversation until the socket closes.
func (s *Session) Run(ctx context.Context) error {
	if err := s.send(ctx, rt.ServerEventBase{Type: rt.ServerEventTypeSessionCreated, EventID: s.ID}); err != nil {
		return err
	}
	defer s.settle()
	for {
		msg, err := s.Sock.Read(ctx)
		if err != nil {
			return nil // the client hung up; that is not a failure
		}
		event, err := Read(msg)
		if err != nil {
			s.fail(ctx, err.Error())
			continue
		}
		if err := s.take(ctx, event); err != nil {
			s.fail(ctx, err.Error())
		}
	}
}

func (s *Session) take(ctx context.Context, event rt.ClientEvent) error {
	switch e := event.(type) {
	case *rt.InputAudioBufferAppendEvent:
		pcm, err := base64.StdEncoding.DecodeString(e.Audio)
		if err != nil {
			return fmt.Errorf("audio is not base64: %w", err)
		}
		return s.hear(ctx, pcm)

	case *FrameEvent:
		jpeg, err := base64.StdEncoding.DecodeString(e.Image)
		if err != nil {
			return fmt.Errorf("frame is not base64: %w", err)
		}
		s.Mind.See(jpeg)
		return nil

	case *rt.InputAudioBufferCommitEvent:
		// An explicit end of turn: the client decided, not the detector.
		go s.reply(context.WithoutCancel(ctx))
		return nil

	case *rt.ResponseCreateEvent:
		go s.reply(context.WithoutCancel(ctx))
		return nil

	case *rt.ResponseCancelEvent:
		s.hush()
		return nil

	case *rt.ConversationItemTruncateEvent:
		// The client says how much of the reply was actually heard. That is
		// the only honest place to cut the record: audio sent is not audio
		// played, and a mind that believes it said what was cut off will
		// answer the next question wrongly.
		s.hush()
		return nil

	case *rt.SessionUpdateEvent:
		return nil

	default:
		return nil
	}
}

// hear feeds audio to the transcriber and the turn detector, and reacts to
// whichever of the two things the floor can do.
func (s *Session) hear(ctx context.Context, pcm []byte) error {
	s.heard.Add(int64(len(pcm)) * 1000 / (Heard * 2))

	// The floor is decided before the audio is handed on, and never behind it.
	// Deciding after would put a network call between a speaker stopping and
	// anything noticing: when the transcriber falls behind — which at four
	// conversations is when, not if — every chunk waits for the one before it,
	// the backlog grows without bound, and the turn is detected late by
	// however far behind the transcriber has fallen. Turn detection is
	// arithmetic on samples already in hand, so it costs nothing to do first.
	floor := s.Turn.Hear(pcm)
	s.Mind.Hear(ctx, pcm)

	switch floor {
	case Held:
		// Speech while the agent is talking is an interruption. Cutting on
		// Held rather than on a separate signal keeps one detector answering
		// one question.
		if s.speaking() {
			s.hush()
			_ = s.send(ctx, rt.ServerEventBase{Type: rt.ServerEventTypeInputAudioBufferSpeechStarted})
		}
	case Yielded:
		_ = s.send(ctx, rt.ServerEventBase{Type: rt.ServerEventTypeInputAudioBufferSpeechStopped})
		s.Turn.Reset()
		go s.reply(context.WithoutCancel(ctx))
	}
	return nil
}

func (s *Session) speaking() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop != nil
}

// hush stops the reply in flight. Silence has to be immediate — a barge-in
// that keeps playing for another sentence is worse than no barge-in, because
// the speaker raises their voice over it.
func (s *Session) hush() {
	s.mu.Lock()
	stop := s.stop
	s.stop = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// reply composes an answer and speaks it, a sentence at a time.
func (s *Session) reply(parent context.Context) {
	ctx, stop := context.WithCancel(parent)
	s.mu.Lock()
	if s.stop != nil { // already answering; the newer utterance wins
		s.mu.Unlock()
		stop()
		return
	}
	s.stop = stop
	s.mu.Unlock()
	defer s.hush()

	item := fmt.Sprintf("%s-%d", s.ID, s.item.Add(1))
	s.spoken.Store(0)

	words, err := s.Mind.Reply(ctx)
	if err != nil {
		s.fail(ctx, err.Error())
		return
	}
	_ = s.send(ctx, rt.ServerEventBase{Type: rt.ServerEventTypeResponseCreated, EventID: item})

	for line := range Sentences(words, 240) {
		if ctx.Err() != nil {
			return // interrupted: stop synthesising what will not be heard
		}
		_ = s.send(ctx, transcript{
			ServerEventBase: rt.ServerEventBase{Type: rt.ServerEventTypeResponseAudioTranscriptDelta, EventID: item},
			ItemID:          item,
			Delta:           line + " ",
		})
		pcm, err := s.Speech.Say(ctx, s.Model, s.Voice, line)
		if err != nil {
			if ctx.Err() == nil {
				s.fail(ctx, "speech: "+err.Error())
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		ms := int64(len(pcm)) * 1000 / (Spoken * 2)
		s.spoken.Add(ms)
		s.said.Add(ms)
		if err := s.send(ctx, audio{
			ServerEventBase: rt.ServerEventBase{Type: rt.ServerEventTypeResponseAudioDelta, EventID: item},
			ItemID:          item,
			Delta:           base64.StdEncoding.EncodeToString(pcm),
		}); err != nil {
			return
		}
	}
	_ = s.send(ctx, rt.ServerEventBase{Type: rt.ServerEventTypeResponseDone, EventID: item})
}

// audio and transcript carry the two deltas the protocol defines but the
// client library, being a client, only ever needs to read.
type audio struct {
	rt.ServerEventBase
	ItemID string `json:"item_id"`
	Delta  string `json:"delta"`
}

type transcript struct {
	rt.ServerEventBase
	ItemID string `json:"item_id"`
	Delta  string `json:"delta"`
}

func (s *Session) send(ctx context.Context, event any) error {
	msg, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.Sock.Write(ctx, msg)
}

func (s *Session) fail(ctx context.Context, why string) {
	// Errors name what the client did wrong and nothing about what is behind
	// the socket. A stack trace on the wire is a map of the inside.
	s.Log.Warn("session error", "id", s.ID, "org", s.Who.Org, "why", why)
	_ = s.send(ctx, map[string]any{
		"type":  rt.ServerEventTypeError,
		"error": map[string]string{"type": "invalid_request_error", "message": clean(why)},
	})
}

func clean(why string) string {
	if i := strings.IndexByte(why, '\n'); i >= 0 {
		why = why[:i]
	}
	if len(why) > 200 {
		why = why[:200]
	}
	return why
}

// settle bills the conversation once, at the end. Seconds are counted as they
// happen but charged together, so a dropped socket cannot lose them.
func (s *Session) settle() {
	if s.Meter == nil {
		return
	}
	s.Meter.Bill(s.Who,
		time.Duration(s.heard.Load())*time.Millisecond,
		time.Duration(s.said.Load())*time.Millisecond)
}
