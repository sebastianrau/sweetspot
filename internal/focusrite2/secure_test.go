package focusrite2

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Generated independently with libsodium-wrappers 0.8.4, using
// crypto_secretstream_xchacha20poly1305_init_push / push (TAG_MESSAGE).
func TestLibsodiumSecretstreamCompatibility(t *testing.T) {
	decode := func(s string) []byte {
		b, e := hex.DecodeString(s)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	header := decode("4d30af608fadfb2465cdb69e3345cdc245a9e556416a6eb9")
	s, e := newPull(header, key)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range []struct{ record, plain string }{
		{"c821e4ea2351839a2fee3103d6ace00a508fb0b06d1b6c74f363124ab7808f110b1d", "FC2 session nonce"},
		{"d1076de0c6590784db07fb5f9cb772de5493df0cf094302824746aac9c99", "OCA frame two"},
	} {
		p, tag, e := s.open(decode(v.record))
		if e != nil || tag != 0 || string(p) != v.plain {
			t.Fatalf("libsodium mismatch: %q tag=%d err=%v", p, tag, e)
		}
	}
	s, _ = newPull(header, key)
	record := decode("c821e4ea2351839a2fee3103d6ace00a508fb0b06d1b6c74f363124ab7808f110b1d")
	record[5] ^= 1
	if _, _, e := s.open(record); e == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestLibsodiumBidirectionalVectors(t *testing.T) {
	var fixture struct {
		Key, Header string
		Records     []struct {
			Plain, Cipher string
			Tag           byte
		}
		KX struct{ Private, ServerPublic, Rx string }
	}
	data, e := os.ReadFile("testdata/libsodium.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(data, &fixture); e != nil {
		t.Fatal(e)
	}
	decode := func(s string) []byte {
		b, e := hex.DecodeString(s)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	encoder, e := newPull(decode(fixture.Header), decode(fixture.Key))
	if e != nil {
		t.Fatal(e)
	}
	decoder, e := newPull(decode(fixture.Header), decode(fixture.Key))
	if e != nil {
		t.Fatal(e)
	}
	for _, record := range fixture.Records {
		plain, cipher := decode(record.Plain), decode(record.Cipher)
		if got := encoder.seal(plain, record.Tag); !bytes.Equal(got, cipher) {
			t.Fatalf("seal mismatch at length %d", len(plain))
		}
		got, tag, e := decoder.open(cipher)
		if e != nil || tag != record.Tag || !bytes.Equal(got, plain) {
			t.Fatalf("open mismatch length=%d tag=%d err=%v", len(plain), tag, e)
		}
	}
	private, e := ecdh.X25519().NewPrivateKey(decode(fixture.KX.Private))
	if e != nil {
		t.Fatal(e)
	}
	var server [32]byte
	copy(server[:], decode(fixture.KX.ServerPublic))
	rx, e := sessionKey(private, server)
	if e != nil || !bytes.Equal(rx, decode(fixture.KX.Rx)) {
		t.Fatalf("key exchange mismatch: %x %v", rx, e)
	}
}

func TestSecureHandshakeAndFragmentedOCAResponse(t *testing.T) {
	cfg, e := (Config{}).WithIdentity()
	if e != nil {
		t.Fatal(e)
	}
	serverPrivate, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ServerPublicKey = hex.EncodeToString(serverPrivate.PublicKey().Bytes())
	identity, _ := cfg.identity()
	var serverKey [32]byte
	copy(serverKey[:], serverPrivate.PublicKey().Bytes())
	rx, e := sessionKey(identity, serverKey)
	if e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+hex.EncodeToString(identity.PublicKey().Bytes()) {
			http.Error(w, "identity", 404)
			return
		}
		ws, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if e != nil {
			result <- e
			return
		}
		defer ws.Close()
		_, first, e := ws.ReadMessage()
		if e != nil {
			result <- e
			return
		}
		if len(first) < 26 {
			result <- fmt.Errorf("short client header")
			return
		}
		pull, e := newPull(first[:24], rx)
		if e != nil {
			result <- e
			return
		}
		decode := func(b []byte) ([]byte, error) {
			if len(b) < 2 || int(be.Uint16(b)) != len(b)-2 {
				return nil, fmt.Errorf("bad encrypted framing")
			}
			p, tag, e := pull.open(b[2:])
			if tag != 0 {
				return nil, fmt.Errorf("unexpected tag")
			}
			return p, e
		}
		clientNonce, e := decode(first[24:])
		if e != nil {
			result <- e
			return
		}
		push, header, e := newPush(rx)
		if e != nil {
			result <- e
			return
		}
		send := func(p []byte) error {
			record := push.seal(p, 0)
			wire := append([]byte(nil), header...)
			header = nil
			wire = append(wire, byte(len(record)>>8), byte(len(record)))
			wire = append(wire, record...)
			// Split both the 24-byte stream header and the encrypted record across
			// binary WebSocket messages, independent from AES70 frame boundaries.
			for len(wire) > 0 {
				n := min(7, len(wire))
				if e := ws.WriteMessage(websocket.BinaryMessage, wire[:n]); e != nil {
					return e
				}
				wire = wire[n:]
			}
			return nil
		}
		nonce := bytes.Repeat([]byte{0x72}, 32)
		if e = send(nonce); e != nil {
			result <- e
			return
		}
		_, b, e := ws.ReadMessage()
		if e != nil {
			result <- e
			return
		}
		echo, e := decode(b)
		if e != nil || !bytes.Equal(echo, nonce) {
			result <- errors.Join(errors.New("wrong server nonce echo"), e)
			return
		}
		if e = send(clientNonce); e != nil {
			result <- e
			return
		}
		_, b, e = ws.ReadMessage()
		if e != nil {
			result <- e
			return
		}
		command, e := decode(b)
		if e != nil {
			result <- e
			return
		}
		if len(command) < 27 || be.Uint32(command[18:]) != 100 {
			result <- fmt.Errorf("wrong encrypted command")
			return
		}
		response := make([]byte, 10)
		body := ocaText("Root")
		be.PutUint32(response, uint32(10+len(body)))
		copy(response[4:8], command[14:18])
		response[9] = 1
		response = append(response, body...)
		wire := frame(3, response)
		for len(wire) > 0 {
			n := min(5, len(wire))
			if e = send(wire[:n]); e != nil {
				result <- e
				return
			}
			wire = wire[n:]
		}
		result <- nil
	}))
	defer srv.Close()
	endpoint := configForURL(srv.URL)
	cfg.Host = endpoint.Host
	cfg.SecurePort = endpoint.SecurePort
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, e := secure(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.ws.Close()
	b, e := c.call(ctx, 100, 1, 5, 0, nil)
	if e != nil {
		t.Fatal(e)
	}
	value, e := text(b)
	if e != nil || value != "Root" {
		t.Fatalf("wrong response %q %v", value, e)
	}
	if e = <-result; e != nil {
		t.Fatal(e)
	}
}
func configForURL(address string) Config {
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(address, "http://"))
	p, _ := strconv.Atoi(port)
	return Config{Host: host, SecurePort: p}
}
