package basicauth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	user = "admin"
	pass = "correct horse battery"
)

type harness struct {
	*Auth
	h     http.Handler
	now   time.Time
	calls atomic.Int32 // requests that reached the protected handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// MinCost keeps the test fast; the config check is tested separately.
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	x := &harness{now: time.Unix(1_800_000_000, 0)}
	x.Auth = New(user, string(hash), slog.New(slog.NewTextHandler(io.Discard, nil)))
	x.Auth.now = func() time.Time { return x.now }
	x.h = x.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.calls.Add(1)
	}))
	return x
}

func (x *harness) get(remote, u, p string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/nodes", nil)
	r.RemoteAddr = remote
	if u != "" || p != "" {
		r.SetBasicAuth(u, p)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Add(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, r)
	return w
}

func TestChallengeAndLogin(t *testing.T) {
	x := newHarness(t)
	w := x.get("203.0.113.1:1000", "", "")
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != realm {
		t.Fatalf("no credentials: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	for _, c := range [][2]string{{user, "wrong"}, {"root", pass}, {"", pass}, {user, ""}} {
		if w := x.get("203.0.113.1:1000", c[0], c[1]); w.Code != 401 {
			t.Errorf("%q/%q: got %d, want 401", c[0], c[1], w.Code)
		}
	}
	if x.calls.Load() != 0 {
		t.Fatal("a rejected request reached the handler")
	}
	for range 3 {
		if w := x.get("203.0.113.1:1000", user, pass); w.Code != 200 {
			t.Fatalf("correct credentials: %d", w.Code)
		}
	}
	if x.calls.Load() != 3 {
		t.Fatalf("handler calls = %d, want 3", x.calls.Load())
	}
}

func TestFailuresAreLimitedPerAddress(t *testing.T) {
	x := newHarness(t)
	const bad, other = "203.0.113.9:1", "198.51.100.7:1"
	for i := range failBurst {
		if w := x.get(bad, user, "guess"); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	w := x.get(bad, user, "guess")
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d", w.Code)
	}
	// Even the right password is refused: without bcrypt it is unknown.
	if w := x.get(bad, user, pass); w.Code != 429 {
		t.Fatalf("right password while limited: %d", w.Code)
	}
	// Another address is unaffected, and once it logged in the cached
	// credentials work from anywhere, including the limited address.
	if w := x.get(other, user, pass); w.Code != 200 {
		t.Fatalf("other address: %d", w.Code)
	}
	if w := x.get(bad, user, pass); w.Code != 200 {
		t.Fatalf("cached credentials from the limited address: %d", w.Code)
	}
	// One more token after failEvery.
	x.now = x.now.Add(failEvery)
	if w := x.get(bad, user, "guess"); w.Code != 401 {
		t.Fatalf("after refill: %d", w.Code)
	}
	if w := x.get(bad, user, "guess"); w.Code != 429 {
		t.Fatalf("refill gave more than one token: %d", w.Code)
	}
}

func TestSuccessDoesNotSpendTokens(t *testing.T) {
	x := newHarness(t)
	for range failBurst - 1 {
		x.get("203.0.113.9:1", user, "guess")
	}
	// The cache is per process; drop it to force bcrypt on every login.
	for range 10 {
		x.mu.Lock()
		x.okSum = nil
		x.mu.Unlock()
		if w := x.get("203.0.113.9:1", user, pass); w.Code != 200 {
			t.Fatalf("login with one token left: %d", w.Code)
		}
	}
}

// A page load right after a restart: many parallel requests with the right
// password must all pass, on one bcrypt.
func TestParallelLoginsShareOneCheck(t *testing.T) {
	x := newHarness(t)
	var wg sync.WaitGroup
	codes := make([]int, 4*failBurst)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = x.get("203.0.113.1:1000", user, pass).Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
}

func TestClientAddr(t *testing.T) {
	for _, c := range []struct {
		remote string
		xff    []string
		want   string
	}{
		{"203.0.113.5:999", nil, "203.0.113.5"},
		// Not from this machine: the header is whatever the client sent.
		{"203.0.113.5:999", []string{"1.2.3.4"}, "203.0.113.5"},
		// From the local reverse proxy: the last entry is the one it added.
		{"127.0.0.1:999", []string{"6.6.6.6, 198.51.100.2"}, "198.51.100.2"},
		{"127.0.0.1:999", []string{"6.6.6.6", "7.7.7.7,198.51.100.3"}, "198.51.100.3"},
		{"[::1]:999", []string{"198.51.100.4"}, "198.51.100.4"},
		{"127.0.0.1:999", []string{"not an address"}, "127.0.0.1"},
		{"127.0.0.1:999", nil, "127.0.0.1"},
		{"[::ffff:203.0.113.5]:999", nil, "203.0.113.5"},
		{"[2001:db8:1:2:3:4:5:6]:999", nil, "2001:db8:1:2::/64"},
		{"127.0.0.1:999", []string{"2001:db8:1:2:aaaa::1"}, "2001:db8:1:2::/64"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientAddr(r); got != c.want {
			t.Errorf("%s %v: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestLimitFollowsForwardedAddress(t *testing.T) {
	x := newHarness(t)
	for range failBurst {
		x.get("127.0.0.1:1", user, "guess", "X-Forwarded-For", "203.0.113.9")
	}
	if w := x.get("127.0.0.1:1", user, "guess", "X-Forwarded-For", "203.0.113.9"); w.Code != 429 {
		t.Fatalf("limited client behind the proxy: %d", w.Code)
	}
	// A spoofed first entry does not move the client to a fresh bucket.
	if w := x.get("127.0.0.1:1", user, "guess", "X-Forwarded-For", "8.8.8.8, 203.0.113.9"); w.Code != 429 {
		t.Fatalf("spoofed X-Forwarded-For: %d", w.Code)
	}
	if w := x.get("127.0.0.1:1", user, "guess", "X-Forwarded-For", "203.0.113.10"); w.Code != 401 {
		t.Fatalf("another client behind the proxy: %d", w.Code)
	}
}

func TestClientTableIsBounded(t *testing.T) {
	x := newHarness(t)
	x.mu.Lock()
	for i := range maxClients {
		x.clients[string(rune(i))] = &client{}
	}
	x.mu.Unlock()
	x.get("203.0.113.9:1", user, "guess")
	x.mu.Lock()
	n := len(x.clients)
	x.mu.Unlock()
	if n != 1 {
		t.Fatalf("clients = %d, want 1 after the table filled up", n)
	}
}

func TestHash(t *testing.T) {
	h, err := Hash(pass)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckHash(h); err != nil {
		t.Fatalf("own hash rejected: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte(pass)) != nil {
		t.Fatal("hash does not match its password")
	}
	weak, _ := bcrypt.GenerateFromPassword([]byte(pass), MinCost-1)
	for _, bad := range []string{"", "secret", "$2a$12$short", string(weak)} {
		if CheckHash(bad) == nil {
			t.Errorf("CheckHash(%q) accepted", bad)
		}
	}
}
