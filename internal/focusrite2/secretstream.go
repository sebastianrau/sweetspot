package focusrite2

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/poly1305" //nolint:staticcheck // Libsodium secretstream requires its separate MAC construction, not standard AEAD framing.
)

// secretStream implements libsodium's record format using x/crypto primitives.
// FC2 has no associated data. Its wire format is NOT standard AEAD framing:
// the encrypted tag occupies a 64-byte MAC block and the message padding uses
// len(ciphertext)%16 (including libsodium's documented historical padding bug).
// Reference: libsodium/crypto_secretstream/xchacha20poly1305/
// secretstream_xchacha20poly1305.c. Cross-implementation vectors cover both
// directions, state transitions, non-block-aligned messages and tampering.
type secretStream struct {
	key   [32]byte
	nonce [12]byte
}

func newPush(key []byte) (*secretStream, []byte, error) {
	header := make([]byte, 24)
	if _, e := rand.Read(header); e != nil {
		return nil, nil, e
	}
	s, e := newPull(header, key)
	return s, header, e
}
func newPull(header, key []byte) (*secretStream, error) {
	if len(header) != 24 || len(key) != 32 {
		return nil, errors.New("invalid FC2 stream key/header")
	}
	subkey, e := chacha20.HChaCha20(key, header[:16])
	if e != nil {
		return nil, e
	}
	s := &secretStream{}
	copy(s.key[:], subkey)
	s.nonce[0] = 1
	copy(s.nonce[4:], header[16:])
	return s, nil
}
func (s *secretStream) cipher() *chacha20.Cipher {
	c, _ := chacha20.NewUnauthenticatedCipher(s.key[:], s.nonce[:])
	return c
}
func (s *secretStream) mac(block, ciphertext []byte) []byte {
	var polyKey [32]byte
	c := s.cipher()
	c.XORKeyStream(polyKey[:], polyKey[:])
	mac := poly1305.New(&polyKey)
	_, _ = mac.Write(block)
	_, _ = mac.Write(ciphertext)
	var padding [16]byte
	_, _ = mac.Write(padding[:len(ciphertext)%16])
	var lengths [16]byte
	binary.LittleEndian.PutUint64(lengths[8:], uint64(64+len(ciphertext)))
	_, _ = mac.Write(lengths[:])
	return mac.Sum(nil)
}
func (s *secretStream) advance(mac []byte, tag byte) {
	for i := range 8 {
		s.nonce[4+i] ^= mac[i]
	}
	counter := binary.LittleEndian.Uint32(s.nonce[:4]) + 1
	binary.LittleEndian.PutUint32(s.nonce[:4], counter)
	if counter == 0 || tag&2 != 0 {
		var material [40]byte
		copy(material[:32], s.key[:])
		copy(material[32:], s.nonce[4:])
		c := s.cipher()
		c.XORKeyStream(material[:], material[:])
		copy(s.key[:], material[:32])
		copy(s.nonce[4:], material[32:])
		binary.LittleEndian.PutUint32(s.nonce[:4], 1)
	}
}
func (s *secretStream) seal(plain []byte, tag byte) []byte {
	var block [64]byte
	block[0] = tag
	c := s.cipher()
	c.SetCounter(1)
	c.XORKeyStream(block[:], block[:])
	out := make([]byte, len(plain)+17)
	out[0] = block[0]
	c.XORKeyStream(out[1:len(plain)+1], plain)
	mac := s.mac(block[:], out[1:len(plain)+1])
	copy(out[len(plain)+1:], mac)
	s.advance(mac, tag)
	return out
}
func (s *secretStream) open(record []byte) ([]byte, byte, error) {
	if len(record) < 17 {
		return nil, 0, errors.New("truncated FC2 encrypted record")
	}
	var block [64]byte
	block[0] = record[0]
	c := s.cipher()
	c.SetCounter(1)
	c.XORKeyStream(block[:], block[:])
	tag := block[0]
	block[0] = record[0]
	ciphertext := record[1 : len(record)-16]
	mac := s.mac(block[:], ciphertext)
	if subtle.ConstantTimeCompare(mac, record[len(record)-16:]) != 1 {
		return nil, 0, errors.New("FC2 record authentication failed")
	}
	plain := make([]byte, len(ciphertext))
	c.XORKeyStream(plain, ciphertext)
	s.advance(mac, tag)
	return plain, tag, nil
}
