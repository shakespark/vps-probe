// Package basicauth guards the web UI with HTTP Basic authentication for
// deployments without Cloudflare Access. It keeps no sessions and adds no
// routes: every request carries the credentials, so it must sit behind an
// HTTPS reverse proxy.
package basicauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	realm = `Basic realm="vps-probe", charset="UTF-8"`

	// Failed attempts per client address: failBurst at once, then one every
	// failEvery.
	failBurst = 5
	failEvery = 12 * time.Second
	// Enough for a botnet's worth of addresses; cleared when full.
	maxClients = 4096
	// bcrypt runs at most this many at a time, so guessing cannot take all
	// the CPU from ingest.
	maxVerifying = 2
	logEvery     = time.Minute
)

// MinCost is the lowest bcrypt cost accepted in the config.
const MinCost = 10

// Hash returns a bcrypt hash for the config's password_hash.
func Hash(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(h), err
}

// CheckHash reports whether s is a bcrypt hash this package accepts.
func CheckHash(s string) error {
	cost, err := bcrypt.Cost([]byte(s))
	if err != nil {
		return err
	}
	if cost < MinCost {
		return bcrypt.InvalidCostError(cost)
	}
	return nil
}

type client struct {
	tokens float64
	seen   time.Time
	logged time.Time
}

type Auth struct {
	user string
	hash []byte
	log  *slog.Logger
	now  func() time.Time

	// Digest of the credentials that last passed bcrypt, under a key that
	// lives only in this process: a page load is a dozen requests and each
	// bcrypt takes tens of milliseconds.
	key [32]byte
	sem chan struct{}

	mu      sync.Mutex
	okSum   []byte
	clients map[string]*client
	// Checks in progress by credentials digest: a page load after a restart
	// is a dozen parallel requests with the same password, which must cost
	// one bcrypt and one failure token, not a dozen.
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	ok   bool
}

func New(user, hash string, log *slog.Logger) *Auth {
	a := &Auth{user: user, hash: []byte(hash), log: log, now: time.Now,
		sem: make(chan struct{}, maxVerifying), clients: map[string]*client{}, flights: map[string]*flight{}}
	rand.Read(a.key[:])
	return a
}

func (a *Auth) sum(user, pass string) []byte {
	m := hmac.New(sha256.New, a.key[:])
	m.Write([]byte(user))
	m.Write([]byte{0})
	m.Write([]byte(pass))
	return m.Sum(nil)
}

// Middleware answers 401 unless the request carries the configured
// credentials, and 429 to an address with too many recent failures.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			a.deny(w)
			return
		}
		switch a.check(r, user, pass) {
		case http.StatusOK:
			next.ServeHTTP(w, r)
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many failed logins, try again later", http.StatusTooManyRequests)
		default:
			a.deny(w)
		}
	})
}

// check returns 200, 401 or 429 for the presented credentials.
func (a *Auth) check(r *http.Request, user, pass string) int {
	sum := a.sum(user, pass)
	a.mu.Lock()
	if a.okSum != nil && hmac.Equal(sum, a.okSum) {
		a.mu.Unlock()
		return http.StatusOK
	}
	if f := a.flights[string(sum)]; f != nil {
		a.mu.Unlock()
		select {
		case <-f.done:
		case <-r.Context().Done():
			return http.StatusUnauthorized
		}
		if f.ok {
			return http.StatusOK
		}
		return http.StatusUnauthorized
	}
	// Spend a failure token before the expensive check and hand it back on
	// success, so concurrent guesses cannot all slip past the limit.
	addr := clientAddr(r)
	if !a.take(addr) {
		a.mu.Unlock()
		return http.StatusTooManyRequests
	}
	f := &flight{done: make(chan struct{})}
	a.flights[string(sum)] = f
	a.mu.Unlock()

	select {
	case a.sem <- struct{}{}:
	case <-r.Context().Done():
		// Gone while queued: nothing was checked, so nothing is spent.
		a.mu.Lock()
		delete(a.flights, string(sum))
		a.refund(addr)
		a.mu.Unlock()
		close(f.done)
		return http.StatusUnauthorized
	}
	// Always run bcrypt, so a wrong user name costs the same as a wrong
	// password.
	passOK := bcrypt.CompareHashAndPassword(a.hash, []byte(pass)) == nil
	<-a.sem
	f.ok = passOK && subtle.ConstantTimeCompare([]byte(user), []byte(a.user)) == 1

	a.mu.Lock()
	delete(a.flights, string(sum))
	logIt := false
	if f.ok {
		a.okSum = sum
		a.refund(addr)
	} else if c := a.clients[addr]; c != nil && a.now().Sub(c.logged) >= logEvery {
		c.logged, logIt = a.now(), true
	}
	a.mu.Unlock()
	close(f.done)
	if logIt {
		a.log.Warn("basic_auth: wrong user name or password", "from", addr)
	}
	if f.ok {
		return http.StatusOK
	}
	return http.StatusUnauthorized
}

func (a *Auth) deny(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", realm)
	http.Error(w, "login required", http.StatusUnauthorized)
}

// take spends one of addr's failure tokens; false means it has none left.
// The caller holds mu.
func (a *Auth) take(addr string) bool {
	now := a.now()
	c := a.clients[addr]
	if c == nil {
		if len(a.clients) >= maxClients {
			clear(a.clients)
		}
		c = &client{tokens: failBurst, seen: now}
		a.clients[addr] = c
	}
	c.tokens = min(failBurst, c.tokens+now.Sub(c.seen).Seconds()/failEvery.Seconds())
	c.seen = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

// refund gives a token back after a successful check. The caller holds mu.
func (a *Auth) refund(addr string) {
	if c := a.clients[addr]; c != nil {
		c.tokens = min(failBurst, c.tokens+1)
	}
}

// clientAddr is the address failures are counted against. A connection from
// this machine is the reverse proxy, which appends the address it saw as the
// last X-Forwarded-For entry; earlier entries are whatever the client sent.
// IPv6 clients are counted per /64.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if ip.Unmap().IsLoopback() {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			last := xff[len(xff)-1]
			last = strings.TrimSpace(last[strings.LastIndexByte(last, ',')+1:])
			if fwd, err := netip.ParseAddr(last); err == nil {
				ip = fwd
			}
		}
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}
