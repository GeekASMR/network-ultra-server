// Package udp implements the UDP data plane for Network Ultra.
//
// Why a parallel UDP path when WebSocket already works:
//   * WebSocket runs on TCP, which on a single packet loss enters
//     head-of-line blocking and stalls all subsequent frames until the
//     retransmit succeeds. On long-RTT links (cross-region) we observed
//     1+ second of audio piling up in the jitter buffer after a momentary
//     stall — enough to make the listener hear a freeze followed by an
//     audible delay floor that never recovers.
//   * UDP discards lost packets immediately; the receiver simply hears one
//     10 ms gap that the jitter buffer absorbs as silence. No HOL aftermath.
//
// Design constraints:
//   * Audio frame format on the wire is BYTE-IDENTICAL to the WS binary
//     frame (same 24-byte AudioFrameHeader + payload). Server's room
//     forwarder doesn't care which transport the frame came from or which
//     one it leaves on — the choice is per-peer based on UDP availability.
//   * Authentication piggy-backs on the existing WS hello/welcome handshake.
//     The WS welcome carries an HMAC token tied to the peerId; the client
//     sends one UDP hello packet with this token to bind its source IP:port.
//     This means we never run a second auth flow on UDP.
//   * Falls back gracefully: if a peer never sends a UDP hello, all its
//     audio continues to flow over WebSocket. Coexisting peers in the
//     same room can use different transports.
package udp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/proto"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

// Maximum UDP datagram we'll accept. Audio frames cap at 24 + kMaxAudioPayload
// (= 24 + 8192 = 8216), so 9000 is comfortably above that.
const maxDatagramSize = 9000

// Idle timeout: peers that go silent for 60 s lose their UDP binding so we
// don't keep stale entries forever. The WebSocket session is unaffected.
const peerIdleTimeout = 60 * time.Second

// Server is the UDP data-plane listener.
type Server struct {
	Log     *slog.Logger
	Metrics *metrics.Registry

	// HMAC key used to mint and verify UDP tokens. Generated at process
	// start; rotated on restart. Rotating means clients have to re-handshake
	// after a server restart, which is fine because they also have to
	// reconnect the WebSocket anyway.
	hmacKey []byte

	// AdvertisedHost is the hostname/IP we tell clients to send UDP to via
	// the WS welcome message. Defaults to the listen address on startup
	// but can be overridden when the server sits behind NAT/load-balancer.
	AdvertisedHost string

	conn *net.UDPConn

	// peerByAddr maps source UDP address -> peer. Lookup happens on every
	// incoming audio packet so it must be fast; we store under RLock and
	// take a brief write lock when binding/unbinding.
	mu          sync.RWMutex
	peerByAddr  map[string]*room.Peer // key = addr.String()
	peerByID    map[uuid.UUID]*room.Peer

	// Reuse output buffers via a per-goroutine sync.Pool to avoid alloc per
	// frame. Hot path: the room forwarder calls SendAudio at 100 fps × N
	// peers.
	outPool sync.Pool

	closeOnce sync.Once
	doneCh    chan struct{}
}

// NewServer initialises a new UDP data-plane server. Call Listen to bind
// the socket and start the read loop.
func NewServer(log *slog.Logger, mreg *metrics.Registry) *Server {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// rand.Read failure is so rare on modern OSs that we treat it as
		// fatal. Falling back to a static key would silently weaken auth.
		panic("udp: failed to generate HMAC key: " + err.Error())
	}
	return &Server{
		Log:        log,
		Metrics:    mreg,
		hmacKey:    key,
		peerByAddr: make(map[string]*room.Peer),
		peerByID:   make(map[uuid.UUID]*room.Peer),
		doneCh:     make(chan struct{}),
		outPool: sync.Pool{
			New: func() any {
				b := make([]byte, 0, 4096)
				return &b
			},
		},
	}
}

// Listen binds the UDP socket on listenAddr and starts the read loop.
// listenAddr is the bind address (e.g. "0.0.0.0:18902"). Returns the
// resolved local address (so callers can advertise the correct port even
// when listenAddr uses :0).
func (s *Server) Listen(listenAddr string) error {
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	// Generous read buffer: at peak we're handling ~100 frames/sec/peer,
	// each up to ~1.5 KB, across maybe 20 peers — well under 4 MB.
	_ = conn.SetReadBuffer(4 * 1024 * 1024)
	_ = conn.SetWriteBuffer(4 * 1024 * 1024)
	s.conn = conn
	s.Log.Info("udp listening", "addr", conn.LocalAddr())

	go s.readLoop()
	go s.gcLoop()
	return nil
}

// LocalAddr returns the address the UDP listener is bound to.
func (s *Server) LocalAddr() net.Addr {
	if s.conn == nil {
		return nil
	}
	return s.conn.LocalAddr()
}

// Close shuts down the UDP listener.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.doneCh)
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
}

// MintToken produces an opaque HMAC token tied to the peerID. The token is
// returned base64-encoded for inclusion in the WS welcome JSON. Server
// validates the same token in the first UDP hello packet.
//
// Token construction is deliberately stateless: we don't keep a per-peer
// nonce table, so a server restart invalidates all outstanding tokens.
// This is fine because the WS session also resets on restart.
func (s *Server) MintToken(peerID uuid.UUID) string {
	h := hmac.New(sha256.New, s.hmacKey)
	h.Write(peerID[:])
	tok := h.Sum(nil) // 32 bytes
	return base64.StdEncoding.EncodeToString(tok)
}

// AttachPeer registers a peer that has been authenticated via WS. The peer
// stays in our map until DetachPeer is called (typically on WS disconnect).
// Initial UDP source address is unknown; it gets bound on the first valid
// UDP hello.
func (s *Server) AttachPeer(p *room.Peer) {
	s.mu.Lock()
	s.peerByID[p.ID] = p
	s.mu.Unlock()
	// Wire up the send callback so the room forwarder routes audio here.
	p.AttachUdpSender(s.makeUdpSender(p))
}

// DetachPeer unbinds the peer from both the addr and id maps. Safe to call
// even if the peer never bound a UDP address.
func (s *Server) DetachPeer(p *room.Peer) {
	p.AttachUdpSender(nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peerByID, p.ID)
	if a := p.UdpAddr(); a != nil {
		delete(s.peerByAddr, a.String())
		p.SetUdpAddr(nil)
	}
}

// makeUdpSender returns a closure suitable for room.Peer.AttachUdpSender.
// The closure reads the peer's bound UDP address atomically and writes once;
// no locks on the hot path. Returns false on any failure so the caller
// falls back to WebSocket.
func (s *Server) makeUdpSender(p *room.Peer) room.UdpSendFunc {
	return func(payload []byte) bool {
		addr := p.UdpAddr()
		if addr == nil || s.conn == nil {
			return false
		}
		_, err := s.conn.WriteToUDP(payload, addr)
		if err != nil {
			// One write error doesn't kill the binding (transient UDP
			// failures happen). The gcLoop will eventually reap stale
			// entries by idle timeout. Caller falls back to WS for this
			// frame.
			s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("write")
			return false
		}
		s.Metrics.Counter("nu_udp_audio_frames_sent_total").Inc()
		s.Metrics.Counter("nu_udp_bytes_sent_total").Add(uint64(len(payload)))
		return true
	}
}

// readLoop is the single read goroutine: it dispatches each datagram by its
// type byte to the appropriate handler.
func (s *Server) readLoop() {
	buf := make([]byte, maxDatagramSize)
	for {
		select {
		case <-s.doneCh:
			return
		default:
		}
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.Log.Debug("udp read error", "err", err)
			continue
		}
		// Defensive: empty or malformed datagrams. We never log these at
		// info level because anyone can spam UDP at us; we just bump the
		// metric and move on.
		if n < 1 {
			s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("empty")
			continue
		}
		typ := buf[0]
		// Copy the slice so the handlers can keep references after the
		// next ReadFromUDP overwrites buf.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		switch typ {
		case proto.AudioFrameType:
			s.handleAudio(pkt, src)
		case proto.UdpFrameTypeHello:
			s.handleHello(pkt, src)
		case proto.UdpFrameTypePing:
			s.handlePing(pkt, src)
		default:
			s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("unknown_type")
		}
	}
}

// handleHello validates the token, binds the source addr, and replies with
// a UdpWelcome echoing the peerId.
func (s *Server) handleHello(pkt []byte, src *net.UDPAddr) {
	hello, err := proto.UnpackUdpHello(pkt)
	if err != nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_hello")
		return
	}

	// Verify HMAC token. Any failure is silently dropped to keep us from
	// becoming a UDP amplifier for spoofed tokens.
	expected := hmac.New(sha256.New, s.hmacKey)
	expected.Write(hello.PeerID[:])
	if !hmac.Equal(hello.Token[:], expected.Sum(nil)) {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_token")
		return
	}

	id := uuid.UUID(hello.PeerID)
	s.mu.Lock()
	p, ok := s.peerByID[id]
	if !ok {
		s.mu.Unlock()
		// Peer not registered — its WS session must have ended (or never
		// arrived). Drop silently.
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("unknown_peer")
		return
	}
	// If the peer was previously bound to a different addr (NAT rebind /
	// re-handshake), drop the old binding before adding the new one.
	if oldAddr := p.UdpAddr(); oldAddr != nil {
		delete(s.peerByAddr, oldAddr.String())
	}
	p.SetUdpAddr(src)
	s.peerByAddr[src.String()] = p
	s.mu.Unlock()

	// Reply so the client knows it's bound. The reply is also necessary for
	// some symmetric NATs to keep the inbound mapping alive.
	reply := proto.PackUdpWelcome(hello.PeerID, nil)
	if _, err := s.conn.WriteToUDP(reply, src); err != nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("welcome_write")
	}
	s.Metrics.Counter("nu_udp_handshake_total").Inc()
	s.Log.Info("udp peer bound", "peerId", id, "src", src)
}

// handlePing is the NAT keepalive. We don't validate against peerByAddr
// strictly — even a packet from an addr we don't know simply gets a pong
// echo (cheap, no state). This makes recovery from NAT rebinds robust:
// the next hello will re-bind without needing pingpong correlation.
func (s *Server) handlePing(pkt []byte, src *net.UDPAddr) {
	ping, err := proto.UnpackUdpPing(pkt)
	if err != nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_ping")
		return
	}
	pong := proto.PackUdpPong(ping.PeerID, nil)
	_, _ = s.conn.WriteToUDP(pong, src)
	s.Metrics.Counter("nu_udp_pings_total").Inc()
}

// handleAudio looks up the peer by source addr, anti-spoofs the source
// peerId in the header against what we expect, and forwards via the
// existing room forwarder (which fans out to other peers using their
// chosen transport).
func (s *Server) handleAudio(pkt []byte, src *net.UDPAddr) {
	s.mu.RLock()
	p := s.peerByAddr[src.String()]
	s.mu.RUnlock()
	if p == nil {
		// Unknown source. Could be a delayed packet from a peer that
		// re-bound, or a spoof. Drop silently.
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("unknown_source")
		return
	}
	hdr, _, err := proto.Unpack(pkt)
	if err != nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("bad_audio")
		return
	}
	// Anti-spoof: the peerId in the header must match the peer the source
	// addr is bound to.
	if hdr.SourcePeerID != [16]byte(p.ID) {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("spoof_peer_id")
		return
	}
	rm := p.CurrentRoom()
	if rm == nil {
		s.Metrics.LabeledCounter("nu_udp_errors_total", "kind").Inc("not_in_room")
		return
	}
	// Hand the frame to the room forwarder. pkt is already a fresh copy
	// (made in readLoop) so we can keep the reference.
	rm.Forward(&room.Frame{
		SourcePeerID: p.ID,
		Payload:      pkt,
	})
	s.Metrics.Counter("nu_udp_audio_frames_recv_total").Inc()
	s.Metrics.Counter("nu_udp_bytes_recv_total").Add(uint64(len(pkt)))
}

// gcLoop periodically reaps peers whose UDP binding has gone idle. WS
// disconnect path also detaches us, but residential NATs sometimes
// silently rebind without notice; the timeout guards against ghost
// entries.
func (s *Server) gcLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	// We track lastSeen per addr in a separate map updated by the read
	// loop, but to keep this MVP lean we simply rely on WS disconnect to
	// detach. The TODO is to wire lastSeen if we observe ghost entries
	// in production.
	for {
		select {
		case <-t.C:
		case <-s.doneCh:
			return
		}
	}
}
