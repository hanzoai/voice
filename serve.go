package voice

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// A browser cannot put a header on a WebSocket. The API takes no options: the
// only things it sends are the URL, cookies for that origin, and a
// subprotocol list. So a bearer either goes in the query string, where it
// lands in every access log and every referrer, or it does not go at all.
//
// It does not go. Instead the caller spends its bearer on an ordinary POST,
// where a header works perfectly, and receives a ticket: a random name for a
// principal this process has already verified and is holding in memory. The
// ticket proves nothing by itself, carries no claims, cannot be verified by
// anyone else, and stops existing the moment it is used or the moment it goes
// stale. Putting THAT in a URL costs nothing.
//
// This is the pattern the estate already uses to authorise a browser onto a
// sandbox terminal socket, and copying it keeps one answer to one question.
const (
	ticketLife = 30 * time.Second
	ticketSize = 24
)

type held struct {
	who Who
	at  time.Time
}

// Desk hands out tickets and takes them back. One use, then gone.
type Desk struct {
	mu   sync.Mutex
	open map[string]held
}

func NewDesk() *Desk { return &Desk{open: map[string]held{}} }

func (d *Desk) Issue(who Who) string {
	b := make([]byte, ticketSize)
	if _, err := rand.Read(b); err != nil {
		panic("voice: no randomness for a ticket: " + err.Error())
	}
	name := base64.RawURLEncoding.EncodeToString(b)
	d.mu.Lock()
	defer d.mu.Unlock()
	// Sweep on the way in: stale tickets are only worth collecting when new
	// ones arrive, so there is no timer to run and nothing to supervise.
	for k, v := range d.open {
		if time.Since(v.at) > ticketLife {
			delete(d.open, k)
		}
	}
	d.open[name] = held{who: who, at: time.Now()}
	return name
}

// Redeem spends a ticket. A second attempt with the same ticket fails, which
// is what makes a copy taken from a log or a browser history worthless.
func (d *Desk) Redeem(name string) (Who, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.open[name]
	if !ok {
		return Who{}, false
	}
	delete(d.open, name)
	if time.Since(h.at) > ticketLife {
		return Who{}, false
	}
	return h.who, true
}

// Voice is the daemon.
type Voice struct {
	Gate   *Gate
	Desk   *Desk
	Room   *Floorspace
	Speech *Speech
	Log    *slog.Logger
	Meter  Meter

	// Think builds the mind and the turn detector for a conversation. It is a
	// field rather than a type so the model behind a session can change
	// without this package knowing what a model is.
	Think func(who Who) (Mind, Turn)

	Model string
	Voice string

	// Origins that may open a socket. A WebSocket is exempt from the
	// same-origin policy, so this list is the only thing standing between a
	// logged-in user and any page on the internet opening a microphone
	// session as them. Empty means same-origin only.
	Origins []string
}

func (v *Voice) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/voice/session", v.session)
	mux.HandleFunc("GET /v1/voice", v.talk)
	mux.HandleFunc("GET /v1/voice/health", v.health)
}

// session spends a bearer and returns a ticket. This is the only place a
// credential is read, and it is read from a header.
func (v *Voice) session(w http.ResponseWriter, r *http.Request) {
	who, err := v.Gate.Read(r.Header.Get("Authorization"), r.Header.Get("X-Org-Id"))
	if err != nil {
		refuse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Ask the doorway now rather than at the upgrade, so a caller learns it is
	// full before it opens a microphone.
	leave, err := v.Room.Enter(who.Org)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	leave() // the real slot is taken at the upgrade; this was only a question

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ticket":     v.Desk.Issue(who),
		"expires_in": int(ticketLife.Seconds()),
		"rate":       Spoken,
		"format":     "pcm16",
		"channels":   1,
	})
}

func (v *Voice) talk(w http.ResponseWriter, r *http.Request) {
	who, ok := v.Desk.Redeem(r.URL.Query().Get("ticket"))
	if !ok {
		refuse(w, http.StatusUnauthorized, "a valid ticket is required")
		return
	}
	leave, err := v.Room.Enter(who.Org)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer leave()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: v.Origins,
		Subprotocols:   []string{"realtime"},
	})
	if err != nil {
		v.Log.Warn("upgrade refused", "why", err)
		return
	}
	defer conn.CloseNow()

	mind, turn := v.Think(who)
	s := &Session{
		ID:     "voice_" + strings.TrimRight(base64.RawURLEncoding.EncodeToString(randomish()), "="),
		Who:    who,
		Sock:   sock{conn},
		Speech: v.Speech,
		Mind:   mind,
		Turn:   turn,
		Meter:  v.Meter,
		Log:    v.Log,
		Model:  v.Model,
		Voice:  v.Voice,
	}
	v.Log.Info("conversation opened", "id", s.ID, "org", who.Org, "user", who.User)
	if err := s.Run(r.Context()); err != nil {
		v.Log.Warn("conversation ended badly", "id", s.ID, "why", err)
	}
	v.Log.Info("conversation closed", "id", s.ID, "org", who.Org)
}

func (v *Voice) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "busy": v.Room.Busy(), "capacity": v.Room.Total,
	})
}

func refuse(w http.ResponseWriter, code int, why string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": why},
	})
}

func randomish() []byte {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return b
}

// sock adapts the websocket to what a Session needs. Audio is binary on the
// wire when the client sends it that way and JSON otherwise; both arrive here
// as bytes and Read sorts them out.
type sock struct{ c *websocket.Conn }

// Big enough for a sentence of 24 kHz audio base64'd, small enough that a
// client cannot spend this process's memory by claiming a huge frame.
const most = 8 << 20

func (s sock) Read(ctx context.Context) ([]byte, error) {
	s.c.SetReadLimit(most)
	_, data, err := s.c.Read(ctx)
	return data, err
}

func (s sock) Write(ctx context.Context, msg []byte) error {
	return s.c.Write(ctx, websocket.MessageText, msg)
}

func (s sock) Close() error { return s.c.Close(websocket.StatusNormalClosure, "") }
