// Package echo is the tunnel probe protocol. An agent sends small
// authenticated requests to a vps-probe-echo responder, usually through a
// port forward, and times the replies. Both sides share a key.
//
// Packet, 31 bytes:
//
//	magic "VPE1" | kind (0 request, 1 reply) | seq (2) | unix nanos (8) |
//	HMAC-SHA256(key, everything before it), first 16 bytes
//
// A reply is the request with kind 1 and a fresh MAC: never larger than the
// request, and a responder never answers a reply, so two responders can't
// be made to bounce packets between each other.
package echo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
)

const (
	Size      = 31
	MinKeyLen = 32
	// Window is the clock difference a responder accepts, which bounds how
	// long a captured request can be replayed.
	Window = 5 * time.Minute

	macOff      = Size - 16
	kindRequest = 0
	kindReply   = 1
)

var magic = []byte("VPE1")

var (
	ErrInvalid = errors.New("not a valid request") // dropped silently
	ErrClock   = errors.New("request timestamp outside the accepted window")
)

func mac(key, b []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(b[:macOff])
	return m.Sum(nil)[:16]
}

func valid(key, b []byte, kind byte) bool {
	return len(b) == Size && string(b[:4]) == string(magic) && b[4] == kind &&
		hmac.Equal(b[macOff:], mac(key, b))
}

// Request builds request seq, stamped with now.
func Request(key []byte, seq uint16, now time.Time) []byte {
	b := make([]byte, Size)
	copy(b, magic)
	b[4] = kindRequest
	binary.BigEndian.PutUint16(b[5:], seq)
	binary.BigEndian.PutUint64(b[7:], uint64(now.UnixNano()))
	copy(b[macOff:], mac(key, b))
	return b
}

// Answer returns the reply to a valid request. ErrClock means the request
// was authentic but stamped too far from now: worth logging, since it means
// the two clocks disagree. Anything else is ErrInvalid.
func Answer(key, req []byte, now time.Time) ([]byte, error) {
	if !valid(key, req, kindRequest) {
		return nil, ErrInvalid
	}
	sent := time.Unix(0, int64(binary.BigEndian.Uint64(req[7:])))
	if d := now.Sub(sent); d > Window || d < -Window {
		return nil, ErrClock
	}
	b := make([]byte, Size)
	copy(b, req)
	b[4] = kindReply
	copy(b[macOff:], mac(key, b))
	return b, nil
}

// ParseReply returns the seq of a valid reply.
func ParseReply(key, b []byte) (uint16, bool) {
	if !valid(key, b, kindReply) {
		return 0, false
	}
	return binary.BigEndian.Uint16(b[5:]), true
}
