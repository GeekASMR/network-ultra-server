package ws

import (
	"sync"

	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

// DeltaBroker fans out room_list_delta to subscribed peers (i.e. clients that
// have an open Browse view). For MVP we track subscribers via Subscribe RPC
// later; v1 simply broadcasts to every connected peer that has issued a
// room_list at least once.
//
// To keep this MVP simple, the broker holds weak references (peer ID + send
// callback) and the dispatch layer registers/unregisters them on join/leave.
type DeltaBroker struct {
	mu          sync.RWMutex
	subscribers map[uuid.UUID]func([]byte)
}

func NewDeltaBroker() *DeltaBroker {
	return &DeltaBroker{subscribers: make(map[uuid.UUID]func([]byte))}
}

func (b *DeltaBroker) Subscribe(peerID uuid.UUID, send func([]byte)) {
	b.mu.Lock()
	b.subscribers[peerID] = send
	b.mu.Unlock()
}

func (b *DeltaBroker) Unsubscribe(peerID uuid.UUID) {
	b.mu.Lock()
	delete(b.subscribers, peerID)
	b.mu.Unlock()
}

func (b *DeltaBroker) Publish(delta room.RoomListDelta) {
	added := make([]proto.RoomListEntry, 0, len(delta.Added))
	for _, e := range delta.Added {
		added = append(added, proto.RoomListEntry{
			RoomName: e.RoomName, PeerCount: e.PeerCount, MaxPeers: e.MaxPeers,
			HasPassword: e.HasPassword, CreatedAt: e.CreatedAt.UnixMilli(),
		})
	}
	updated := make([]proto.RoomListEntry, 0, len(delta.Updated))
	for _, e := range delta.Updated {
		updated = append(updated, proto.RoomListEntry{
			RoomName: e.RoomName, PeerCount: e.PeerCount, MaxPeers: e.MaxPeers,
			HasPassword: e.HasPassword, CreatedAt: e.CreatedAt.UnixMilli(),
		})
	}
	body, err := proto.Encode(proto.TypeRoomListDelta, "", proto.RoomListDeltaData{
		Added: added, Removed: delta.Removed, Updated: updated,
	})
	if err != nil {
		return
	}

	b.mu.RLock()
	subs := make([]func([]byte), 0, len(b.subscribers))
	for _, s := range b.subscribers {
		subs = append(subs, s)
	}
	b.mu.RUnlock()
	for _, s := range subs {
		s(body)
	}
}
