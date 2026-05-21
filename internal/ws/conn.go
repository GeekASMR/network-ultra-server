package ws

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/coder/websocket"
)

// Conn wraps a nhooyr WebSocket with a writeQueue + write goroutine so multiple
// senders (forwarder + control handlers) don't race on the underlying conn.
type Conn struct {
	c   *websocket.Conn
	log *slog.Logger

	writeCh chan outgoing
	doneCh  chan struct{}
	wg      sync.WaitGroup

	closeOnce sync.Once
}

type outgoing struct {
	mt   websocket.MessageType
	data []byte
}

func newConn(c *websocket.Conn, log *slog.Logger) *Conn {
	cn := &Conn{
		c:       c,
		log:     log,
		writeCh: make(chan outgoing, connWriteQueueDepth),
		doneCh:  make(chan struct{}),
	}
	cn.wg.Add(1)
	go cn.writeLoop()
	return cn
}

// SendFunc returns a thread-safe send function suitable for room.Peer.AttachSender.
func (cn *Conn) SendFunc() func([]byte, bool) error {
	return func(p []byte, isBinary bool) error {
		mt := websocket.MessageText
		if isBinary {
			mt = websocket.MessageBinary
		}
		return cn.write(mt, p)
	}
}

func (cn *Conn) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return cn.c.Read(ctx)
}

func (cn *Conn) write(mt websocket.MessageType, data []byte) error {
	select {
	case cn.writeCh <- outgoing{mt: mt, data: data}:
		return nil
	case <-cn.doneCh:
		return errors.New("conn closed")
	default:
		// queue full → slow consumer; close conn to free room forwarder.
		cn.close()
		return errors.New("write queue full")
	}
}

func (cn *Conn) writeLoop() {
	defer cn.wg.Done()
	for {
		select {
		case msg, ok := <-cn.writeCh:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := cn.c.Write(ctx, msg.mt, msg.data)
			cancel()
			if err != nil {
				cn.log.Debug("ws write failed", "err", err)
				cn.close()
				return
			}
		case <-cn.doneCh:
			return
		}
	}
}

func (cn *Conn) close() {
	cn.closeOnce.Do(func() {
		close(cn.doneCh)
		_ = cn.c.Close(websocket.StatusNormalClosure, "")
	})
}

// Close blocks until the write loop exits. Used in HandleHTTP defer.
func (cn *Conn) Close() {
	cn.close()
	cn.wg.Wait()
}
