package voice

import (
	"fmt"
	"sync"
)

// Capacity is how many conversations the speech service sustains at once.
//
// It is four. That is measured, not chosen: the transcriber runs a pool of
// four decode workers, and past four the backlog grows without bound rather
// than the acks merely slowing down. The service itself enforces nothing — it
// has no notion of a caller and refuses nobody — so a queue there becomes
// latency here, and latency is the one thing a conversation cannot absorb.
// Refusing a fifth session is kinder than degrading five.
const Capacity = 4

// Share is the most of that capacity any one tenant may hold. Without it the
// first org to arrive takes every slot and every other tenant is refused by a
// neighbour rather than by its own usage.
const Share = 2

// Floorspace admits conversations while there is room and refuses them when
// there is not.
//
// This is a doorway, not a queue. A voice session that waits has already
// failed: the speaker is holding a live microphone. So the answer is
// immediate and it is either yes or no.
type Floorspace struct {
	mu    sync.Mutex
	held  int
	byOrg map[string]int
	Total int
	Each  int
}

func NewFloorspace(total, each int) *Floorspace {
	return &Floorspace{byOrg: map[string]int{}, Total: total, Each: each}
}

// Enter takes a slot for an org. The returned func gives it back and is safe
// to call more than once, because a session can end in more ways than it began.
func (f *Floorspace) Enter(org string) (leave func(), err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held >= f.Total {
		return nil, fmt.Errorf("all %d conversation slots are busy", f.Total)
	}
	if f.byOrg[org] >= f.Each {
		return nil, fmt.Errorf("this org already holds its %d conversations", f.Each)
	}
	f.held++
	f.byOrg[org]++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.held--
			if f.byOrg[org]--; f.byOrg[org] <= 0 {
				delete(f.byOrg, org)
			}
		})
	}, nil
}

// Busy is how many conversations are live, for the health endpoint.
func (f *Floorspace) Busy() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}
