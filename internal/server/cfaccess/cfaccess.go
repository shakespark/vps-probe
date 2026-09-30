// Package cfaccess verifies the Cloudflare Access JWT that Access adds to
// every request it lets through (Cf-Access-Jwt-Assertion). With it on,
// the web UI answers only requests that passed an Access login, even if
// the listener is accidentally reachable some other way.
package cfaccess

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	Header       = "Cf-Access-Jwt-Assertion"
	leeway       = 30 * time.Second
	refetchMin   = time.Minute // unknown kid: refetch keys at most this often
	refreshEvery = time.Hour
	retryEvery   = 30 * time.Second
	logEvery     = time.Minute
)

type Verifier struct {
	iss, aud string
	certsURL string
	client   *http.Client
	log      *slog.Logger
	now      func() time.Time

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchMu   sync.Mutex
	lastFetch time.Time

	logMu   sync.Mutex
	lastLog map[string]time.Time
}

// New verifies tokens for the Access application with the given AUD tag in
// team teamDomain (e.g. myteam.cloudflareaccess.com).
func New(teamDomain, aud string, log *slog.Logger) *Verifier {
	return &Verifier{
		iss:      "https://" + teamDomain,
		aud:      aud,
		certsURL: "https://" + teamDomain + "/cdn-cgi/access/certs",
		client:   &http.Client{Timeout: 10 * time.Second},
		log:      log,
		now:      time.Now,
		keys:     map[string]*rsa.PublicKey{},
		lastLog:  map[string]time.Time{},
	}
}

// Run loads the signing keys and keeps them fresh. Until the first load
// succeeds every request is rejected; startup doesn't wait for it.
func (v *Verifier) Run(ctx context.Context) {
	for {
		wait := refreshEvery
		if err := v.fetch(ctx); err != nil {
			v.log.Error("cf_access: loading signing keys failed; web requests are rejected until it works",
				"url", v.certsURL, "err", err)
			wait = retryEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (v *Verifier) fetch(ctx context.Context) error {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	v.lastFetch = v.now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certsURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("no RSA keys in response")
	}
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	return nil
}

func (v *Verifier) key(kid string) *rsa.PublicKey {
	v.mu.RLock()
	k := v.keys[kid]
	v.mu.RUnlock()
	if k != nil {
		return k
	}
	// Keys rotate: an unknown kid may be a new key. Refetch, rate-limited
	// so that tokens with made-up kids can't hammer Cloudflare.
	v.fetchMu.Lock()
	recent := v.now().Sub(v.lastFetch) < refetchMin
	v.fetchMu.Unlock()
	if recent {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := v.fetch(ctx); err != nil {
		v.log.Warn("cf_access: refetching signing keys", "err", err)
		return nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.keys[kid]
}

// Verify checks signature (RS256 only), audience, issuer and validity time.
func (v *Verifier) Verify(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("malformed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &hdr); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return fmt.Errorf("alg %q not accepted", hdr.Alg)
	}
	key := v.key(hdr.Kid)
	if key == nil {
		return fmt.Errorf("unknown signing key %q", hdr.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("malformed signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return errors.New("bad signature")
	}

	var claims struct {
		Aud audience `json:"aud"`
		Iss string   `json:"iss"`
		Exp int64    `json:"exp"`
		Nbf int64    `json:"nbf"`
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return fmt.Errorf("claims: %w", err)
	}
	now := v.now()
	switch {
	case !claims.Aud.has(v.aud):
		return errors.New("wrong audience (token is for another Access application)")
	case claims.Iss != v.iss:
		return fmt.Errorf("wrong issuer %q", claims.Iss)
	case claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(leeway)):
		return errors.New("expired")
	case claims.Nbf != 0 && now.Add(leeway).Before(time.Unix(claims.Nbf, 0)):
		return errors.New("not valid yet")
	}
	return nil
}

func decodePart(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// audience is a JWT "aud": a string or a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) has(s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// Middleware rejects any request without a valid Access token.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(Header)
		var err error
		if token == "" {
			err = errors.New("no " + Header + " header (request did not come through Cloudflare Access)")
		} else {
			err = v.Verify(token)
		}
		if err != nil {
			// Keyed by the first word only: reasons can contain attacker-chosen text.
			v.rateLog(strings.SplitN(err.Error(), " ", 2)[0], "cf_access: request rejected", "reason", err, "path", r.URL.Path)
			http.Error(w, "forbidden: Cloudflare Access login required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (v *Verifier) rateLog(key, msg string, args ...any) {
	v.logMu.Lock()
	now := v.now()
	if now.Sub(v.lastLog[key]) < logEvery {
		v.logMu.Unlock()
		return
	}
	if len(v.lastLog) > 100 {
		clear(v.lastLog)
	}
	v.lastLog[key] = now
	v.logMu.Unlock()
	v.log.Warn(msg, args...)
}
