package wire

import (
	"bytes"
	"errors"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789a"

func TestRoundTrip(t *testing.T) {
	a, err := NewAEAD(testToken, "hk-1")
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello report")
	pkt, err := Seal(a, TypeReport, "hk-1", msg)
	if err != nil {
		t.Fatal(err)
	}
	h, err := ParseHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != TypeReport || h.Node != "hk-1" {
		t.Fatalf("header = %+v", h)
	}
	got, err := h.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q", got)
	}
}

func TestNonceIsRandom(t *testing.T) {
	a, _ := NewAEAD(testToken, "n")
	p1, _ := Seal(a, TypeReport, "n", []byte("x"))
	p2, _ := Seal(a, TypeReport, "n", []byte("x"))
	if bytes.Equal(p1, p2) {
		t.Fatal("two packets with identical bytes")
	}
}

func TestTamper(t *testing.T) {
	a, _ := NewAEAD(testToken, "hk-1")
	pkt, _ := Seal(a, TypeReport, "hk-1", []byte("payload"))
	for i := range pkt {
		bad := bytes.Clone(pkt)
		bad[i] ^= 0x01
		h, err := ParseHeader(bad)
		if err != nil {
			continue // framing rejected, fine
		}
		// Flipping a node byte changes the key the server would pick; with the
		// original key the AAD no longer matches either way.
		if _, err := h.Open(a); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
}

func TestReflectedAckRejectedAsReport(t *testing.T) {
	a, _ := NewAEAD(testToken, "hk-1")
	pkt, _ := Seal(a, TypeAck, "hk-1", []byte("ack"))
	pkt[1] = TypeReport
	h, err := ParseHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Open(a); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
}

func TestWrongKey(t *testing.T) {
	a, _ := NewAEAD(testToken, "hk-1")
	pkt, _ := Seal(a, TypeReport, "hk-1", []byte("payload"))
	h, _ := ParseHeader(pkt)

	other, _ := NewAEAD("another-token-another-token-another-token-x", "hk-1")
	if _, err := h.Open(other); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong token: err = %v", err)
	}
	// Same token, different node derives a different key.
	sameTokOtherNode, _ := NewAEAD(testToken, "jp-1")
	if _, err := h.Open(sameTokOtherNode); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong node key: err = %v", err)
	}
}

func TestParseHeaderRejects(t *testing.T) {
	a, _ := NewAEAD(testToken, "hk-1")
	good, _ := Seal(a, TypeReport, "hk-1", []byte("p"))

	cases := map[string][]byte{
		"empty":             nil,
		"tiny":              {1, 1},
		"truncated":         good[:len(good)-20],
		"version":           append([]byte{9}, good[1:]...),
		"too long":          make([]byte, MaxPacket+1),
		"node len 0":        append([]byte{1, 1, 0}, make([]byte, 64)...),
		"node chars":        append(append([]byte{1, 1, 2}, "a/"...), make([]byte, 64)...),
		"node len overflow": {1, 1, 200, 'a'},
	}
	for name, pkt := range cases {
		if _, err := ParseHeader(pkt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSealLimits(t *testing.T) {
	a, _ := NewAEAD(testToken, "n")
	max := MaxPacket - Overhead - 1
	if _, err := Seal(a, TypeReport, "n", make([]byte, max)); err != nil {
		t.Fatalf("max size: %v", err)
	}
	if _, err := Seal(a, TypeReport, "n", make([]byte, max+1)); !errors.Is(err, ErrLong) {
		t.Fatalf("over max: err = %v", err)
	}
	if _, err := Seal(a, TypeReport, "bad node", nil); !errors.Is(err, ErrNode) {
		t.Fatalf("bad node: err = %v", err)
	}
}

func TestValidNode(t *testing.T) {
	for _, s := range []string{"a", "hk-1", "us_west.2", "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"} {
		if !ValidNode(s) {
			t.Errorf("%q rejected", s)
		}
	}
	for _, s := range []string{"", "a b", "中文", "a/b", "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456"} {
		if ValidNode(s) {
			t.Errorf("%q accepted", s)
		}
	}
}
