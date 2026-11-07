package focusrite2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"golang.org/x/crypto/blake2b"
)

type encrypted struct {
	push, pull  *secretStream
	key         []byte
	header      []byte
	buffer      []byte
	initialized bool
}

func secure(ctx context.Context, cfg Config) (*oca, error) {
	identity, e := cfg.identity()
	if e != nil {
		return nil, e
	}
	server, e := key(cfg.ServerPublicKey)
	if e != nil {
		return nil, e
	}
	rx, e := sessionKey(identity, server)
	if e != nil {
		return nil, e
	}
	c, e := dial(ctx, cfg.address(cfg.SecurePort)+hex.EncodeToString(identity.PublicKey().Bytes()))
	if e != nil {
		return nil, e
	}
	s := &encrypted{key: rx}
	s.push, s.header, e = newPush(rx)
	if e != nil {
		c.ws.Close()
		return nil, e
	}
	rawSend, rawReceive := c.send, c.receive
	c.send = func(p []byte) error {
		out := append([]byte(nil), s.header...)
		s.header = nil
		for len(p) > 0 {
			n := min(1283, len(p))
			record := s.push.seal(p[:n], 0)
			out = append(out, byte(len(record)>>8), byte(len(record)))
			out = append(out, record...)
			p = p[n:]
		}
		return rawSend(out)
	}
	c.receive = func() ([]byte, error) {
		for {
			if !s.initialized && len(s.buffer) >= 24 {
				var e error
				s.pull, e = newPull(s.buffer[:24], s.key)
				if e != nil {
					return nil, e
				}
				s.buffer = s.buffer[24:]
				s.initialized = true
			}
			if s.initialized && len(s.buffer) >= 2 {
				n := int(be.Uint16(s.buffer))
				if n < 17 || n > 1300 {
					return nil, errors.New("invalid FC2 encrypted record size")
				}
				if len(s.buffer) >= n+2 {
					p, tag, e := s.pull.open(s.buffer[2 : n+2])
					s.buffer = s.buffer[n+2:]
					if e != nil {
						return nil, e
					}
					if tag > 2 {
						return nil, errors.New("FC2 session ended or rekeyed unexpectedly")
					}
					return p, nil
				}
			}
			b, e := rawReceive()
			if e != nil {
				return nil, e
			}
			if len(s.buffer)+len(b) > 1<<20 {
				return nil, errors.New("FC2 encrypted buffer limit exceeded")
			}
			s.buffer = append(s.buffer, b...)
		}
	}
	fail := func(e error) (*oca, error) { c.ws.Close(); return nil, e }
	stop := context.AfterFunc(ctx, func() { _ = c.ws.Close() })
	defer stop()
	var nonce [32]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return fail(e)
	}
	if e = c.send(nonce[:]); e != nil {
		return fail(e)
	}
	serverNonce, e := c.receive()
	if e != nil {
		return fail(e)
	}
	if len(serverNonce) != 32 || bytes.Equal(serverNonce, nonce[:]) {
		return fail(errors.New("invalid FC2 server nonce"))
	}
	if e = c.send(serverNonce); e != nil {
		return fail(e)
	}
	echo, e := c.receive()
	if e != nil {
		return fail(e)
	}
	if !bytes.Equal(echo, nonce[:]) {
		return fail(errors.New("FC2 did not confirm client nonce"))
	}
	return c, nil
}

func sessionKey(identity *ecdh.PrivateKey, server [32]byte) ([]byte, error) {
	remote, e := identity.Curve().NewPublicKey(server[:])
	if e != nil {
		return nil, e
	}
	shared, e := identity.ECDH(remote)
	if e != nil {
		return nil, e
	}
	h, _ := blake2b.New512(nil)
	_, _ = h.Write(shared)
	_, _ = h.Write(identity.PublicKey().Bytes())
	_, _ = h.Write(server[:])
	return h.Sum(nil)[:32], nil
}
