// Package focusrite2 implements the FC2 remote onboarding and control transport.
package focusrite2

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

var be = binary.BigEndian

type ocaStatusError struct {
	object       uint32
	level, index uint16
	status       byte
}

func (e *ocaStatusError) Error() string {
	return fmt.Sprintf("OCA %d.%d on %d: status %d", e.level, e.index, e.object, e.status)
}

// oca serializes requests. The caller owns the connection and must not call
// methods concurrently; keepalives are sent by the receive loop while waiting.
type oca struct {
	ws      *websocket.Conn
	send    func([]byte) error
	receive func() ([]byte, error)
	seq     uint32
	pending []byte
}

func dial(ctx context.Context, url string) (*oca, error) {
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, resp, err := d.DialContext(ctx, url, http.Header{"Origin": []string{"capacitor://localhost"}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("FC2 WebSocket: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	c := &oca{ws: ws}
	c.send = func(b []byte) error {
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return ws.WriteMessage(websocket.BinaryMessage, b)
	}
	c.receive = func() ([]byte, error) {
		for {
			kind, b, err := ws.ReadMessage()
			if err != nil {
				return nil, err
			}
			if kind == websocket.BinaryMessage {
				return b, nil
			}
		}
	}
	return c, nil
}

func frame(kind byte, body []byte) []byte {
	b := make([]byte, 10, len(body)+10)
	b[0] = 0x3b
	be.PutUint16(b[1:], 1)
	be.PutUint32(b[3:], uint32(9+len(body)))
	b[7] = kind
	be.PutUint16(b[8:], 1)
	return append(b, body...)
}

func (c *oca) call(ctx context.Context, object uint32, level, index uint16, count byte, args []byte) ([]byte, error) {
	c.seq++
	p := make([]byte, 17, len(args)+17)
	be.PutUint32(p, uint32(17+len(args)))
	be.PutUint32(p[4:], c.seq)
	be.PutUint32(p[8:], object)
	be.PutUint16(p[12:], level)
	be.PutUint16(p[14:], index)
	p[16] = count
	if err := c.send(frame(1, append(p, args...))); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.ws.Close() })
	defer stop()
	// Approval may wait for the user. Keep the OCA session alive meanwhile.
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = c.send(frame(4, []byte{0, 3}))
			}
		}
	}()
	defer func() { close(done); <-exited }()
	// send is only used by the keepalive goroutine until this call returns.
	for {
		b, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		if b[7] != 3 {
			continue
		}
		rest := b[10:]
		for n := int(be.Uint16(b[8:])); n > 0; n-- {
			if len(rest) < 10 {
				return nil, errors.New("truncated OCA response")
			}
			size := int(be.Uint32(rest))
			if size < 10 || size > len(rest) {
				return nil, errors.New("invalid OCA response size")
			}
			if be.Uint32(rest[4:]) == c.seq {
				if rest[8] != 0 {
					return nil, &ocaStatusError{object: object, level: level, index: index, status: rest[8]}
				}
				return append([]byte(nil), rest[10:size]...), nil
			}
			rest = rest[size:]
		}
	}
}

func (c *oca) readFrame() ([]byte, error) {
	for {
		if len(c.pending) >= 10 {
			if c.pending[0] != 0x3b {
				return nil, errors.New("invalid OCA sync byte")
			}
			n := int(be.Uint32(c.pending[3:])) + 1
			if n < 10 || n > 1<<20 {
				return nil, errors.New("invalid OCA frame length")
			}
			if len(c.pending) >= n {
				b := c.pending[:n]
				c.pending = c.pending[n:]
				return b, nil
			}
		}
		b, err := c.receive()
		if err != nil {
			return nil, err
		}
		if len(c.pending)+len(b) > 1<<20 {
			return nil, errors.New("OCA buffer limit exceeded")
		}
		c.pending = append(c.pending, b...)
	}
}

func blob(b []byte) []byte {
	p := make([]byte, 2, len(b)+2)
	be.PutUint16(p, uint16(len(b)))
	return append(p, b...)
}
func text(b []byte) (string, error) {
	if len(b) < 2 {
		return "", io.ErrUnexpectedEOF
	}
	n := int(be.Uint16(b))
	b = b[2:]
	pos := 0
	for range n {
		if pos >= len(b) {
			return "", io.ErrUnexpectedEOF
		}
		_, size := utf8.DecodeRune(b[pos:])
		if size == 1 && b[pos] >= 128 {
			return "", errors.New("invalid OCA UTF-8")
		}
		pos += size
	}
	if pos != len(b) {
		return "", errors.New("unexpected OCA string suffix")
	}
	return string(b), nil
}
