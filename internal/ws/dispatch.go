package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/coder/websocket"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

const (
	serverVersion       = "1.0.0"
	writeTimeout        = 5 * time.Second
	helloTimeout        = 10 * time.Second
	pingTimeout         = 30 * time.Second
	connWriteQueueDepth = 512
)

type Server struct {
	Reg     *room.Registry
	Metrics *metrics.Registry
	Log     *slog.Logger

	// Limits
	MaxConnections int

	// Optional: the WS subprotocol clients must request.
	Subprotocol string

	// stats
	curConns int64
	mu       sync.Mutex
}

// HandleHTTP upgrades incoming HTTP into a WebSocket connection and runs it.
func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	// Per-process cap.
	s.mu.Lock()
	if int(s.curConns) >= s.MaxConnections {
		s.mu.Unlock()
		http.Error(w, "server full", http.StatusServiceUnavailable)
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrServerFull)
		return
	}
	s.curConns++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.curConns--
		s.mu.Unlock()
	}()

	opts := &websocket.AcceptOptions{
		InsecureSkipVerify: true, // we don't validate origin; clients are VST hosts
	}
	if s.Subprotocol != "" {
		opts.Subprotocols = []string{s.Subprotocol}
	}

	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.Log.Warn("ws upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	defer c.Close(websocket.StatusInternalError, "shutting down")

	s.Metrics.Counter("nu_ws_connections_total").Inc()

	conn := newConn(c, s.Log)
	defer conn.close()

	if err := s.run(r.Context(), conn, r.RemoteAddr); err != nil {
		s.Log.Debug("session ended", "err", err, "remote", r.RemoteAddr)
	}
}

// run drives a single WS session: hello → loop dispatching messages.
func (s *Server) run(parent context.Context, conn *Conn, remote string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// 1. Wait for hello.
	hctx, hcancel := context.WithTimeout(ctx, helloTimeout)
	defer hcancel()

	mt, payload, err := conn.read(hctx)
	if err != nil {
		return err
	}
	if mt != websocket.MessageText {
		return s.protoError(conn, "first frame must be hello (text)")
	}

	env, err := proto.Decode(payload)
	if err != nil || env.Type != proto.TypeHello || env.V > proto.ProtocolVersion {
		return s.protoError(conn, "bad hello")
	}
	var hello proto.HelloData
	if err := json.Unmarshal(env.Data, &hello); err != nil {
		return s.protoError(conn, "bad hello data")
	}
	if !validUsername(hello.Username) {
		s.sendError(conn, env.ID, proto.ErrBadUsername, "username invalid")
		return errors.New("bad username")
	}

	peer := room.NewPeer(hello.Username, "")
	s.Metrics.Gauge("nu_active_peers").Inc()
	defer s.Metrics.Gauge("nu_active_peers").Dec()

	peer.AttachSender(conn.SendFunc())
	defer peer.DetachSender()

	if err := s.send(conn, proto.TypeWelcome, env.ID, proto.WelcomeData{
		PeerID:        peer.ID.String(),
		ServerVersion: serverVersion,
	}); err != nil {
		return err
	}

	host, _, _ := net.SplitHostPort(remote)
	s.Log.Info("peer authenticated", "peerId", peer.ID, "username", hello.Username, "host", host)

	// Server-side WebSocket ping every 15s. Many residential / mobile
	// networks silently drop idle TCP after ~30s; without a periodic
	// keepalive packet the stateful firewall RSTs us, the client sees
	// "peer close frame" and reconnects in a tight loop.
	pingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = conn.c.Ping(pctx)
				cancel()
			}
		}
	}()
	defer close(pingDone)

	// 2. Main loop.
	for {
		mt, payload, err := conn.read(ctx)
		if err != nil {
			s.cleanupPeer(peer, "disconnect")
			return err
		}
		switch mt {
		case websocket.MessageText:
			s.handleControl(ctx, conn, peer, payload)
		case websocket.MessageBinary:
			s.handleAudio(peer, payload)
		}
	}
}

func (s *Server) handleControl(ctx context.Context, conn *Conn, peer *room.Peer, payload []byte) {
	env, err := proto.Decode(payload)
	if err != nil {
		s.sendError(conn, "", proto.ErrProtocolError, "bad json")
		return
	}
	switch env.Type {
	case proto.TypeRoomCreate:
		s.handleRoomCreate(conn, peer, env)
	case proto.TypeRoomJoin:
		s.handleRoomJoin(conn, peer, env)
	case proto.TypeRoomLeave:
		s.handleRoomLeave(conn, peer, env)
	case proto.TypeRoomList:
		s.handleRoomList(conn, peer, env)
	case proto.TypePeerMute:
		s.handlePeerMute(conn, peer, env)
	case proto.TypeSubscribe:
		s.handleSubscribe(conn, peer, env)
	case proto.TypePing:
		s.handlePing(conn, env)
	default:
		s.sendError(conn, env.ID, proto.ErrProtocolError, "unknown type "+env.Type)
	}
	_ = ctx
}

func (s *Server) handleAudio(peer *room.Peer, payload []byte) {
	hdr, audio, err := proto.Unpack(payload)
	if err != nil {
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrProtocolError)
		return
	}
	rm := peer.CurrentRoom()
	if rm == nil {
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrNotInRoom)
		return
	}
	// Anti-spoof: source must be self.
	if hdr.SourcePeerID != [16]byte(peer.ID) {
		s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(proto.ErrProtocolError)
		return
	}

	// Copy payload because nhooyr reuses the read buffer.
	cp := make([]byte, len(payload))
	copy(cp, payload)

	frame := &room.Frame{
		SourcePeerID: peer.ID,
		Payload:      cp,
	}
	if !rm.Forward(frame) {
		s.Metrics.LabeledCounter("nu_audio_frames_dropped_total", "reason").Inc("backpressure")
		return
	}
	s.Metrics.Counter("nu_audio_frames_forwarded_total").Inc()
	s.Metrics.Counter("nu_audio_bytes_forwarded_total").Add(uint64(len(payload)))
	_ = audio
	_ = hdr
}

func (s *Server) handleRoomCreate(conn *Conn, peer *room.Peer, env proto.Envelope) {
	if peer.CurrentRoom() != nil {
		s.sendError(conn, env.ID, proto.ErrAlreadyInRoom, "leave first")
		return
	}
	var d proto.RoomCreateData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	if d.Visibility != "public" && d.Visibility != "private" {
		d.Visibility = "private"
	}
	rm, err := s.Reg.Create(d.RoomName, d.Visibility, d.Password)
	if err != nil {
		switch err {
		case room.ErrRoomNameTaken:
			s.sendError(conn, env.ID, proto.ErrRoomNameTaken, err.Error())
		case room.ErrServerFull:
			s.sendError(conn, env.ID, proto.ErrServerFull, err.Error())
		default:
			s.sendError(conn, env.ID, proto.ErrInternalError, err.Error())
		}
		return
	}
	s.Metrics.Counter("nu_room_create_total").Inc()
	s.Metrics.Gauge("nu_active_rooms").Set(int64(s.Reg.CountRooms()))

	// Auto-join the creator (role defaults to "send" — unimportant for control flow).
	s.joinPeerToRoom(conn, peer, rm, "send", env.ID)
}

func (s *Server) handleRoomJoin(conn *Conn, peer *room.Peer, env proto.Envelope) {
	if peer.CurrentRoom() != nil {
		s.sendError(conn, env.ID, proto.ErrAlreadyInRoom, "leave first")
		return
	}
	var d proto.RoomJoinData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	rm := s.Reg.Find(d.RoomName)
	if rm == nil {
		s.sendError(conn, env.ID, proto.ErrRoomNotFound, "no such room")
		return
	}
	if err := rm.CheckPassword(d.Password); err != nil {
		s.sendError(conn, env.ID, proto.ErrBadPassword, "bad password")
		return
	}
	role := d.Role
	if role != "send" && role != "recv" {
		role = "send"
	}
	s.joinPeerToRoom(conn, peer, rm, role, env.ID)
}

func (s *Server) joinPeerToRoom(conn *Conn, peer *room.Peer, rm *room.Room, role, reqID string) {
	peer.Role = role
	others, err := rm.Add(peer)
	if err != nil {
		s.sendError(conn, reqID, proto.ErrRoomFull, "room full")
		return
	}

	// Convert peer snapshots → wire PeerInfo.
	peers := make([]proto.PeerInfo, 0, len(others))
	for _, p := range others {
		peers = append(peers, proto.PeerInfo{
			PeerID:   p.ID.String(),
			Username: p.Username,
			Role:     p.Role,
			Muted:    p.Muted,
			JoinedAt: p.JoinedAt.UnixMilli(),
		})
	}

	_ = s.send(conn, proto.TypeRoomJoined, reqID, proto.RoomJoinedData{
		RoomID:   rm.ID.String(),
		RoomName: rm.Name,
		Peers:    peers,
	})

	// Notify other peers in the room.
	s.broadcastToOthers(rm, peer.ID, proto.TypePeerJoined, "", proto.PeerInfo{
		PeerID:   peer.ID.String(),
		Username: peer.Username,
		Role:     peer.Role,
		Muted:    peer.Muted(),
		JoinedAt: peer.JoinedAt.UnixMilli(),
	})

	s.Reg.PublishUpdate(rm)
}

func (s *Server) handleRoomLeave(conn *Conn, peer *room.Peer, env proto.Envelope) {
	rm := peer.CurrentRoom()
	if rm == nil {
		s.sendError(conn, env.ID, proto.ErrNotInRoom, "not in room")
		return
	}
	s.cleanupPeer(peer, "leave")
	_ = s.send(conn, proto.TypeRoomLeft, env.ID, proto.RoomLeftData{Reason: "leave"})
}

func (s *Server) handleRoomList(conn *Conn, peer *room.Peer, env proto.Envelope) {
	list := s.Reg.PublicList()
	wire := make([]proto.RoomListEntry, 0, len(list))
	for _, e := range list {
		wire = append(wire, proto.RoomListEntry{
			RoomName:    e.RoomName,
			PeerCount:   e.PeerCount,
			MaxPeers:    e.MaxPeers,
			HasPassword: e.HasPassword,
			CreatedAt:   e.CreatedAt.UnixMilli(),
		})
	}
	_ = s.send(conn, proto.TypeRoomListResult, env.ID, proto.RoomListResultData{Rooms: wire})
	_ = peer
}

func (s *Server) handlePeerMute(conn *Conn, peer *room.Peer, env proto.Envelope) {
	var d proto.PeerMuteData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	peer.SetMuted(d.Muted)
	if rm := peer.CurrentRoom(); rm != nil {
		s.broadcastToOthers(rm, peer.ID, proto.TypePeerMuteChanged, "", proto.PeerMuteChangedData{
			PeerID: peer.ID.String(),
			Muted:  d.Muted,
		})
	}
	_ = conn
}

func (s *Server) handleSubscribe(conn *Conn, peer *room.Peer, env proto.Envelope) {
	var d proto.SubscribeData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.sendError(conn, env.ID, proto.ErrProtocolError, "bad data")
		return
	}
	ids := make([]uuid.UUID, 0, len(d.SourcePeerIDs))
	for _, s := range d.SourcePeerIDs {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	peer.SetSubscribed(ids)
	_ = conn
}

func (s *Server) handlePing(conn *Conn, env proto.Envelope) {
	var d proto.PingData
	_ = json.Unmarshal(env.Data, &d)
	_ = s.send(conn, proto.TypePong, env.ID, proto.PongData{
		ClientTS: d.TS,
		ServerTS: time.Now().UnixMilli(),
	})
}

func (s *Server) cleanupPeer(peer *room.Peer, reason string) {
	rm := peer.CurrentRoom()
	if rm == nil {
		return
	}
	rm.Remove(peer.ID)
	s.Metrics.Gauge("nu_active_rooms").Set(int64(s.Reg.CountRooms()))
	s.broadcastToOthers(rm, peer.ID, proto.TypePeerLeft, "", proto.PeerLeftData{
		PeerID: peer.ID.String(),
		Reason: reason,
	})
	s.Reg.PublishUpdate(rm)
}

// broadcastToOthers sends a control message to every peer in the room except
// the source peer.
func (s *Server) broadcastToOthers(rm *room.Room, except uuid.UUID, typ, id string, payload any) {
	body, err := proto.Encode(typ, id, payload)
	if err != nil {
		return
	}
	rm.ForEachPeer(func(p *room.Peer) {
		if p.ID == except {
			return
		}
		_ = p.SendText(body)
	})
}

func (s *Server) send(conn *Conn, typ, id string, payload any) error {
	body, err := proto.Encode(typ, id, payload)
	if err != nil {
		return err
	}
	return conn.write(websocket.MessageText, body)
}

func (s *Server) sendError(conn *Conn, reqID, code, msg string) {
	_ = s.send(conn, proto.TypeError, reqID, proto.ErrorData{Code: code, Message: msg})
	s.Metrics.LabeledCounter("nu_errors_total", "code").Inc(code)
}

func (s *Server) protoError(conn *Conn, reason string) error {
	s.sendError(conn, "", proto.ErrProtocolError, reason)
	return errors.New(reason)
}

func validUsername(u string) bool {
	if len(u) == 0 || len(u) > 32 {
		return false
	}
	return true // tighter validation can be added; keep MVP loose
}
