package focusrite2

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// Pair requests desktop approval and consumes one QR payload from the mobile
// bridge. The identity must have been saved before calling Pair.
func Pair(ctx context.Context, c Config, qr <-chan string, status func(string)) error {
	if e := c.Validate(); e != nil {
		return e
	}
	private, e := key(c.PrivateKey)
	if e != nil {
		return e
	}
	server, e := key(c.ServerPublicKey)
	if e != nil {
		return e
	}
	identity, e := c.identity()
	if e != nil {
		return e
	}
	conn, e := dial(ctx, c.address(c.OnboardingPort))
	if e != nil {
		return e
	}
	defer conn.ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.ws.Close() })
	defer stop()
	call := func(index uint16, args []byte, count byte) (string, error) {
		b, e := conn.call(ctx, 4096, 3, index, count, args)
		if e != nil {
			return "", e
		}
		return text(b)
	}
	state, e := call(3, nil, 0)
	if e != nil {
		return e
	}
	if state == "ready" {
		var nonce [24]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			return e
		}
		encrypted := box.Seal(nonce[:], []byte("Sweetspot"), &nonce, &server, &private)
		status("Approve Sweetspot in Focusrite Control 2 on this computer.")
		decision, e := call(1, append(blob(identity.PublicKey().Bytes()), blob(encrypted)...), 2)
		if e != nil {
			return e
		}
		if decision != "approved" {
			return fmt.Errorf("FC2 approval: %s", decision)
		}
	} else if state != "scanning" {
		return fmt.Errorf("FC2 is not ready for pairing (%s)", state)
	}
	status("Open this link on your phone and scan the QR code shown by Focusrite Control 2.")
	keepalive := time.NewTicker(time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-keepalive.C:
			if e = conn.send(frame(4, []byte{0, 3})); e != nil {
				return e
			}
		case value, ok := <-qr:
			if !ok {
				return errors.New("QR input closed")
			}
			payload, e := DecodePayload(value)
			if e != nil {
				return e
			}
			decision, e := call(2, blob(payload), 1)
			if e != nil {
				return e
			}
			if decision != "approved" {
				return fmt.Errorf("FC2 QR verification: %s", decision)
			}
			status("Paired with Focusrite Control 2.")
			return nil
		}
	}
}

func DecodePayload(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > 8192 {
		return nil, errors.New("invalid FC2 QR payload length")
	}
	b, e := hex.DecodeString(s)
	if e != nil {
		return nil, errors.New("the FC2 QR code must contain hexadecimal data")
	}
	return b, nil
}
