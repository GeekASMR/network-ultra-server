package room

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const (
	audioChBuffer       = 256
	roomEmptyDestroyDur = 30 * time.Second
	roomIdleNoJoinDur   = 60 * time.Second
)

var (
	ErrRoomFull       = errors.New("room full")
	ErrBadPassword    = errors.New("bad password")
	ErrAlreadyInRoom  = errors.New("peer already in a room")
	ErrPeerNotInRoom  = errors.New("peer not in this room")
)

// Frame is a fully-packed audio frame (header + payload). It is reference-
// counted via the Done() callback because a single inbound frame fans out
// to many recv peers.
type Frame struct {
	SourcePeerID uuid.UUID
	Payload      []byte
	// Done is called once the frame is no longer needed by any consumer.
	// May be nil (caller does not need to release).
	Done func()
}

// Room holds peers and a forwarder goroutine.
type Room struct {
	ID           uuid.UUID
	Name         string
	NameLower    string
	Visibility   string // "public" | "private"
	HasPassword  bool
	passwordHash []byte // bcrypt; nil if no password
	MaxPeers     int
	CreatedAt    time.Time

	mu    sync.RWMutex
	peers map[uuid.UUID]*Peer

	audioCh chan *Frame
	closeCh chan struct{}
	wg      sync.WaitGroup

	// destroy timer / disposable flags managed by Registry.
	destroyTimer *time.Timer

	// Registry back-reference for housekeeping (set by Registry.Create).
	registry *Registry
}

// Snapshot of peer infos at one point in time, for room_joined / peer list.
type PeerSnapshot struct {
	ID       uuid.UUID
	Username string
	Role     string
	Muted    bool
	JoinedAt time.Time
}

func (r *Room) Snapshot() []PeerSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PeerSnapshot, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, PeerSnapshot{
			ID:       p.ID,
			Username: p.Username,
			Role:     p.Role,
			Muted:    p.Muted(),
			JoinedAt: p.JoinedAt,
		})
	}
	return out
}

// PeerCount returns the current peer count (cheap, atomic-ish).
func (r *Room) PeerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// CheckPassword returns nil when the supplied password matches (or the room
// has no password).
func (r *Room) CheckPassword(plain string) error {
	if !r.HasPassword {
		return nil
	}
	if err := bcrypt.CompareHashAndPassword(r.passwordHash, []byte(plain)); err != nil {
		return ErrBadPassword
	}
	return nil
}

// Add attempts to add a peer to the room. Returns the peer snapshot list
// (excluding the new peer) for the room_joined response.
func (r *Room) Add(p *Peer) ([]PeerSnapshot, error) {
	r.mu.Lock()
	if len(r.peers) >= r.MaxPeers {
		r.mu.Unlock()
		return nil, ErrRoomFull
	}
	r.peers[p.ID] = p
	if r.destroyTimer != nil {
		r.destroyTimer.Stop()
		r.destroyTimer = nil
	}

	others := make([]PeerSnapshot, 0, len(r.peers)-1)
	for _, q := range r.peers {
		if q.ID == p.ID {
			continue
		}
		others = append(others, PeerSnapshot{
			ID:       q.ID,
			Username: q.Username,
			Role:     q.Role,
			Muted:    q.Muted(),
			JoinedAt: q.JoinedAt,
		})
	}
	r.mu.Unlock()

	p.setRoom(r)
	return others, nil
}

// Remove drops a peer. If the room becomes empty, schedules a delayed destroy.
func (r *Room) Remove(peerID uuid.UUID) {
	r.mu.Lock()
	p, ok := r.peers[peerID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.peers, peerID)
	empty := len(r.peers) == 0
	r.mu.Unlock()

	p.setRoom(nil)

	if empty && r.registry != nil {
		r.scheduleEmptyDestroy()
	}
}

// scheduleEmptyDestroy starts a timer to remove the room from the registry
// roomEmptyDestroyDur after going empty. Caller holds no lock.
func (r *Room) scheduleEmptyDestroy() {
	r.mu.Lock()
	if r.destroyTimer != nil {
		r.mu.Unlock()
		return
	}
	r.destroyTimer = time.AfterFunc(roomEmptyDestroyDur, func() {
		r.registry.destroy(r)
	})
	r.mu.Unlock()
}

// Forward enqueues a frame onto the room's forwarder. Returns false if the
// channel is full (caller drops the frame).
func (r *Room) Forward(f *Frame) bool {
	select {
	case r.audioCh <- f:
		return true
	default:
		if f.Done != nil {
			f.Done()
		}
		return false
	}
}

func (r *Room) start() {
	r.wg.Add(1)
	go r.forwardLoop()
}

func (r *Room) close() {
	select {
	case <-r.closeCh:
	default:
		close(r.closeCh)
	}
	r.wg.Wait()
}

func (r *Room) forwardLoop() {
	defer r.wg.Done()
	for {
		select {
		case f := <-r.audioCh:
			r.fanOut(f)
		case <-r.closeCh:
			// drain pending
			for {
				select {
				case f := <-r.audioCh:
					if f != nil && f.Done != nil {
						f.Done()
					}
				default:
					return
				}
			}
		}
	}
}

func (r *Room) fanOut(f *Frame) {
	r.mu.RLock()
	peers := make([]*Peer, 0, len(r.peers))
	for _, p := range r.peers {
		if p.ID == f.SourcePeerID {
			continue
		}
		if p.Role != "recv" {
			continue
		}
		if !p.IsSubscribed(f.SourcePeerID) {
			continue
		}
		peers = append(peers, p)
	}
	r.mu.RUnlock()

	for _, p := range peers {
		// Best-effort send. Slow consumers do not block fast ones because
		// p.SendBinary writes to the per-conn writeQueue and times out there.
		_ = p.SendBinary(f.Payload)
	}
	if f.Done != nil {
		f.Done()
	}
}

// idleDestroyAfterCreate fires roomIdleNoJoinDur after creation if no peer
// ever joined. Used to clean ghost rooms (creator crashes during join).
func (r *Room) startIdleDestroyTimer() {
	time.AfterFunc(roomIdleNoJoinDur, func() {
		r.mu.RLock()
		empty := len(r.peers) == 0
		r.mu.RUnlock()
		if empty && r.registry != nil {
			r.registry.destroy(r)
		}
	})
}

// hashPassword turns a plaintext password into its bcrypt hash. Empty input
// returns nil (no password).
func hashPassword(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
}


// ForEachPeer invokes fn for every peer in the room. fn is called while the
// read lock is held; do not block in fn.
func (r *Room) ForEachPeer(fn func(*Peer)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		fn(p)
	}
}
