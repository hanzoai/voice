package voice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

// tape is a socket that behaves as coder/websocket does on the one point that
// matters here: a write whose context is already done closes the connection.
type tape struct {
	mu     sync.Mutex
	sent   []string
	closed bool
}

func (t *tape) Read(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (t *tape) Write(ctx context.Context, msg []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return net.ErrClosed
	}
	if ctx.Err() != nil {
		t.closed = true
		return ctx.Err()
	}
	var head struct{ Type string }
	_ = json.Unmarshal(msg, &head)
	t.sent = append(t.sent, head.Type)
	return nil
}

func (t *tape) Close() error { return nil }

func (t *tape) open() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed
}

// thinking is a mind that has begun to answer and has said nothing yet, the
// moment a speaker most often talks over. It answers, or fails, only once told
// to stop.
type thinking struct {
	started chan struct{}
	fails   bool
}

func (m *thinking) Hear(context.Context, []byte) {}
func (m *thinking) See([]byte)                   {}
func (m *thinking) Reply(ctx context.Context) (<-chan string, error) {
	close(m.started)
	if m.fails {
		<-ctx.Done()
		return nil, errors.New("nothing was said")
	}
	out := make(chan string)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()
	return out, nil
}

// TestSpeakingOverAReplyEndsTheTurnNotTheConversation: barge-in cancels the reply,
// and whatever the cancelled reply does on its way out must leave the socket open
// for the next turn.
func TestSpeakingOverAReplyEndsTheTurnNotTheConversation(t *testing.T) {
	for _, fails := range []bool{false, true} {
		sock := &tape{}
		mind := &thinking{started: make(chan struct{}), fails: fails}
		s := &Session{ID: "s", Sock: sock, Mind: mind, Turn: &Hush{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

		done := make(chan struct{})
		go func() {
			s.reply(context.Background())
			close(done)
		}()
		<-mind.started
		s.hush() // the speaker talks over it
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a spoken-over reply never finished")
		}
		if !sock.open() {
			t.Fatalf("mind fails=%v: the cancelled reply closed the socket; sent %v", fails, sock.sent)
		}
		for _, sent := range sock.sent {
			if sent == "error" {
				t.Fatalf("mind fails=%v: a turn that was spoken over was reported as an error", fails)
			}
		}
	}
}

// TestALeavingReplyDoesNotCancelTheNextOne: the reply that was spoken over
// finishes after the next has taken the floor, and must leave it alone.
func TestALeavingReplyDoesNotCancelTheNextOne(t *testing.T) {
	s := &Session{}
	stop1, stop2 := func() {}, func() {}
	s.stop, s.turn = stop1, 1
	s.hush()                  // spoken over
	s.stop, s.turn = stop2, 2 // the next reply took the floor
	s.finish(1, func() {})
	if s.stop == nil {
		t.Fatal("the old reply leaving cleared the new reply's floor")
	}
	s.finish(2, func() {})
	if s.stop != nil {
		t.Fatal("a reply leaving did not give up its own floor")
	}
}
