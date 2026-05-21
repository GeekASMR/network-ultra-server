package room

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// SendFunc is the callback the WS layer registers so the room can push
// outbound frames to a peer without coupling to the websocket package.
type SendFunc func(textPayload []byte, isBinary bool) error

// Peer represents a single WebSocket-attached participant inside a Room.
type Peer struct {
	ID       uuid.UUID
	Username string
	Role     string // "send" | "recv"
	JoinedAt time.Time

	// muted is per-peer self-mute (Send peer flagging itself).
	muted atomic.Bool

	// subscribed is the set of source peers this Recv peer wants to hear.
	// Stored as atomic.Pointer to a map[uuid.UUID]struct{}; nil = subscribe to all.
	subscribed atomic.Pointer[map[uuid.UUID]struct{}]

	// send is the WS-layer-provided callback. Nil if the peer is detached.
	mu   sync.RWMutex
	send SendFunc

	// Last room reference (atomic-friendly) so we can clean up on disconnect.
	curRoom atomic.Pointer[Room]
}

func NewPeer(username, role string) *Peer {
	return &Peer{
		ID:       uuid.New(),
		Username: username,
		Role:     role,
		JoinedAt: time.Now(),
	}
}

func (p *Peer) Muted() bool       { return p.muted.Load() }
func (p *Peer) SetMuted(v bool)   { p.muted.Store(v) }

// IsSubscribed returns true if this peer (presumed Recv) wants to hear src.
// nil subscription map means "subscribe all".
func (p *Peer) IsSubscribed(src uuid.UUID) bool {
	subs := p.subscribed.Load()
	if subs == nil {
		return true
	}
	_, ok := (*subs)[src]
	return ok
}

func (p *Peer) SetSubscribed(ids []uuid.UUID) {
	if ids == nil {
		p.subscribed.Store(nil)
		return
	}
	m := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	p.subscribed.Store(&m)
}

// AttachSender registers the WS-layer callback. Replaces any prior callback.
func (p *Peer) AttachSender(s SendFunc) {
	p.mu.Lock()
	p.send = s
	p.mu.Unlock()
}

// DetachSender clears the callback (called on disconnect).
func (p *Peer) DetachSender() {
	p.mu.Lock()
	p.send = nil
	p.mu.Unlock()
}

// SendText pushes a control message to this peer. Returns nil if the peer
// is detached (no error to fail-fast; caller decides).
func (p *Peer) SendText(payload []byte) error {
	p.mu.RLock()
	s := p.send
	p.mu.RUnlock()
	if s == nil {
		return nil
	}
	return s(payload, false)
}

func (p *Peer) SendBinary(payload []byte) error {
	p.mu.RLock()
	s := p.send
	p.mu.RUnlock()
	if s == nil {
		return nil
	}
	return s(payload, true)
}

// CurrentRoom returns the room this peer is in, or nil.
func (p *Peer) CurrentRoom() *Room { return p.curRoom.Load() }

func (p *Peer) setRoom(r *Room) { p.curRoom.Store(r) }
