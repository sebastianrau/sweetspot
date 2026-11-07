package focusrite2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/nacl/box"
)

// This peer verifies the wire format and decrypts the approval name, rather
// than mocking Pair's internal calls.
func TestPairWireHandshake(t *testing.T) {
	serverPrivate, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := (Config{ServerPublicKey: hex.EncodeToString(serverPrivate.PublicKey().Bytes())}).WithIdentity()
	if e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		ws, e := u.Upgrade(w, r, nil)
		if e != nil {
			result <- e
			return
		}
		defer ws.Close()
		for step, index := range []uint16{3, 1, 2} {
			_, b, e := ws.ReadMessage()
			if e != nil {
				result <- e
				return
			}
			if len(b) < 27 || b[0] != 0x3b || b[7] != 1 || be.Uint32(b[18:]) != 4096 || be.Uint16(b[22:]) != 3 || be.Uint16(b[24:]) != index {
				result <- fmt.Errorf("invalid authentication command %x", b)
				return
			}
			args := b[27:]
			if step == 1 {
				if len(args) < 36 || be.Uint16(args) != 32 {
					result <- fmt.Errorf("invalid client key field")
					return
				}
				var pub, priv [32]byte
				copy(pub[:], args[2:34])
				copy(priv[:], serverPrivate.Bytes())
				n := int(be.Uint16(args[34:]))
				encrypted := args[36:]
				if len(encrypted) != n || n < 40 {
					result <- fmt.Errorf("invalid encrypted name field")
					return
				}
				var nonce [24]byte
				copy(nonce[:], encrypted[:24])
				name, ok := box.Open(nil, encrypted[24:], &nonce, &pub, &priv)
				if !ok || string(name) != "Sweetspot" {
					result <- fmt.Errorf("approval name not authenticated")
					return
				}
			}
			if step == 2 && !bytes.Equal(args, []byte{0, 4, 0xde, 0xad, 0xbe, 0xef}) {
				result <- fmt.Errorf("unexpected QR payload: %x", args)
				return
			}
			value := "approved"
			if step == 0 {
				value = "ready"
			}
			response := make([]byte, 10)
			be.PutUint32(response, uint32(12+len(value)))
			copy(response[4:8], b[14:18])
			response[9] = 1
			response = append(response, blob([]byte(value))...)
			if e = ws.WriteMessage(websocket.BinaryMessage, frame(3, response)); e != nil {
				result <- e
				return
			}
		}
		result <- nil
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	cfg.Host = host
	cfg.OnboardingPort, _ = strconv.Atoi(port)
	cfg.SecurePort = cfg.OnboardingPort
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	qr := make(chan string, 1)
	qr <- "deadbeef"
	if e = Pair(ctx, cfg, qr, func(string) {}); e != nil {
		t.Fatal(e)
	}
	if e = <-result; e != nil {
		t.Fatal(e)
	}
}

func TestPairCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer ws.Close()
		for {
			if _, _, e = ws.ReadMessage(); e != nil {
				return
			}
		}
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	cfg, _ := (Config{Host: host, ServerPublicKey: strings.Repeat("12", 32), SecurePort: 1}).WithIdentity()
	cfg.OnboardingPort, _ = strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if e := Pair(ctx, cfg, make(chan string), func(string) {}); e == nil {
		t.Fatal("expected cancellation")
	}
}
