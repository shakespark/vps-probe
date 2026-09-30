// Package wire implements the encrypted UDP packet format shared by agent and
// server:
//
//	ver(1) | type(1) | node_len(1) | node | nonce(24) | ciphertext+tag(16)
//
// The header up to and including node is authenticated as additional data,
// so tampering with it, or reflecting an Ack back as a Report, fails to open.
package wire

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	Version = 1

	TypeReport byte = 1
	TypeAck    byte = 2

	// MaxPacket keeps datagrams below common path MTUs to avoid fragmentation.
	MaxPacket = 1200

	MaxNodeLen = 32

	keySalt = "vps-probe/v1"
)

// Overhead is the packet size excluding node name and plaintext.
const Overhead = 3 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

var (
	ErrShort   = errors.New("wire: packet too short")
	ErrLong    = errors.New("wire: packet too long")
	ErrVersion = errors.New("wire: unsupported version")
	ErrNode    = errors.New("wire: invalid node id")
	ErrOpen    = errors.New("wire: authentication failed")
)

// ValidNode reports whether id is usable as a node id: 1-32 chars of
// [A-Za-z0-9._-].
func ValidNode(id string) bool {
	if len(id) == 0 || len(id) > MaxNodeLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// NewAEAD derives the per-node key from the shared token.
func NewAEAD(token, node string) (cipher.AEAD, error) {
	if !ValidNode(node) {
		return nil, ErrNode
	}
	key, err := hkdf.Key(sha256.New, []byte(token), []byte(keySalt), node, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(key)
}

// Seal builds an encrypted packet.
func Seal(aead cipher.AEAD, typ byte, node string, plaintext []byte) ([]byte, error) {
	if !ValidNode(node) {
		return nil, ErrNode
	}
	hdrLen := 3 + len(node)
	size := hdrLen + aead.NonceSize() + len(plaintext) + aead.Overhead()
	if size > MaxPacket {
		return nil, fmt.Errorf("%w: %d bytes", ErrLong, size)
	}
	pkt := make([]byte, hdrLen+aead.NonceSize(), size)
	pkt[0] = Version
	pkt[1] = typ
	pkt[2] = byte(len(node))
	copy(pkt[3:], node)
	nonce := pkt[hdrLen:]
	rand.Read(nonce)
	return aead.Seal(pkt, nonce, plaintext, pkt[:hdrLen]), nil
}

// Header is the unauthenticated part of a packet. Callers use Node to look up
// the key and must then call Open; nothing in Header is trustworthy before that.
type Header struct {
	Type byte
	Node string
	aad  []byte
	body []byte // nonce + ciphertext
}

// ParseHeader validates framing without touching the ciphertext.
func ParseHeader(pkt []byte) (Header, error) {
	if len(pkt) > MaxPacket {
		return Header{}, ErrLong
	}
	if len(pkt) < 3 {
		return Header{}, ErrShort
	}
	if pkt[0] != Version {
		return Header{}, ErrVersion
	}
	n := int(pkt[2])
	hdrLen := 3 + n
	if len(pkt) < hdrLen+chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return Header{}, ErrShort
	}
	node := string(pkt[3:hdrLen])
	if !ValidNode(node) {
		return Header{}, ErrNode
	}
	return Header{Type: pkt[1], Node: node, aad: pkt[:hdrLen], body: pkt[hdrLen:]}, nil
}

// Open authenticates and decrypts the packet body.
func (h Header) Open(aead cipher.AEAD) ([]byte, error) {
	ns := aead.NonceSize()
	if len(h.body) < ns+aead.Overhead() {
		return nil, ErrShort
	}
	pt, err := aead.Open(nil, h.body[:ns], h.body[ns:], h.aad)
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}
