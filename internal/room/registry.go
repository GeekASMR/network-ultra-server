package room

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRoomNameTaken = errors.New("room name taken")
	ErrServerFull    = errors.New("max rooms reached")
	ErrRoomNotFound  = errors.New("room not found")
)

type Registry struct {
	maxRooms        int
	maxPeersPerRoom int

	mu    sync.RWMutex
	rooms map[string]*Room // key = lowercased room name

	// notifySubscribers is called whenever the public room list changes.
	notifyCb func(delta RoomListDelta)
}

type RoomListDelta struct {
	Added   []RoomListEntry
	Removed []string
	Updated []RoomListEntry
}

type RoomListEntry struct {
	RoomName    string
	PeerCount   int
	MaxPeers    int
	HasPassword bool
	CreatedAt   time.Time
}

func NewRegistry(maxRooms, maxPeersPerRoom int) *Registry {
	return &Registry{
		maxRooms:        maxRooms,
		maxPeersPerRoom: maxPeersPerRoom,
		rooms:           make(map[string]*Room),
	}
}

// SetDeltaListener installs a single callback for public-room list changes.
// In v1 the WS layer registers one listener that fans out to subscribers.
func (r *Registry) SetDeltaListener(cb func(RoomListDelta)) {
	r.mu.Lock()
	r.notifyCb = cb
	r.mu.Unlock()
}

// Create attempts to create a room.
func (r *Registry) Create(name, visibility, password string) (*Room, error) {
	if name == "" {
		return nil, errors.New("empty name")
	}
	nameLower := strings.ToLower(name)

	r.mu.Lock()
	if _, exists := r.rooms[nameLower]; exists {
		r.mu.Unlock()
		return nil, ErrRoomNameTaken
	}
	if len(r.rooms) >= r.maxRooms {
		r.mu.Unlock()
		return nil, ErrServerFull
	}

	hash, err := hashPassword(password)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}

	room := &Room{
		ID:           uuid.New(),
		Name:         name,
		NameLower:    nameLower,
		Visibility:   visibility,
		HasPassword:  hash != nil,
		passwordHash: hash,
		MaxPeers:     r.maxPeersPerRoom,
		CreatedAt:    time.Now(),
		peers:        make(map[uuid.UUID]*Peer),
		audioCh:      make(chan *Frame, audioChBuffer),
		closeCh:      make(chan struct{}),
		registry:     r,
	}
	r.rooms[nameLower] = room
	cb := r.notifyCb
	r.mu.Unlock()

	room.start()
	room.startIdleDestroyTimer()

	if visibility == "public" && cb != nil {
		cb(RoomListDelta{Added: []RoomListEntry{r.entryOf(room)}})
	}
	return room, nil
}

// Find looks up a room by name (case-insensitive).
func (r *Registry) Find(name string) *Room {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rooms[strings.ToLower(name)]
}

// destroy removes the room from the registry and stops its forwarder.
// Called by Room.scheduleEmptyDestroy or idle timer.
func (r *Registry) destroy(room *Room) {
	r.mu.Lock()
	if existing, ok := r.rooms[room.NameLower]; ok && existing == room {
		delete(r.rooms, room.NameLower)
	} else {
		r.mu.Unlock()
		return
	}
	cb := r.notifyCb
	visibility := room.Visibility
	name := room.Name
	r.mu.Unlock()

	room.close()

	if visibility == "public" && cb != nil {
		cb(RoomListDelta{Removed: []string{name}})
	}
}

// PublicList returns a snapshot of all public rooms for room_list_result.
func (r *Registry) PublicList() []RoomListEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RoomListEntry, 0)
	for _, rm := range r.rooms {
		if rm.Visibility == "public" {
			out = append(out, r.entryOf(rm))
		}
	}
	return out
}

// entryOf builds a list entry for a room. Caller holds at least RLock.
func (r *Registry) entryOf(rm *Room) RoomListEntry {
	return RoomListEntry{
		RoomName:    rm.Name,
		PeerCount:   rm.PeerCount(),
		MaxPeers:    rm.MaxPeers,
		HasPassword: rm.HasPassword,
		CreatedAt:   rm.CreatedAt,
	}
}

// PublishUpdate fires an "updated" delta. Used by WS layer when peer count
// changes in a public room.
func (r *Registry) PublishUpdate(rm *Room) {
	r.mu.RLock()
	cb := r.notifyCb
	pub := rm.Visibility == "public"
	entry := r.entryOf(rm)
	r.mu.RUnlock()
	if pub && cb != nil {
		cb(RoomListDelta{Updated: []RoomListEntry{entry}})
	}
}

// CountRooms returns the current room count.
func (r *Registry) CountRooms() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.rooms)
}

// CountPeers returns total peers across all rooms (used for /healthz).
func (r *Registry) CountPeers() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, rm := range r.rooms {
		n += rm.PeerCount()
	}
	return n
}
