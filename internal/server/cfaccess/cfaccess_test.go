package cfaccess

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	team = "myteam.cloudflareaccess.com"
	aud  = "4714c1358e65fe4b408ad6d432a5f878f08194bdb4752441fd56faefa9b2b6f2"
)

type jwks struct {
	keys atomic.Value // map[string]*rsa.PrivateKey
	hits atomic.Int32
	down atomic.Bool
	srv  *httptest.Server
}

func newJWKS(t *testing.T, kids ...string) *jwks {
	j := &jwks{}
	j.set(t, kids...)
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		j.hits.Add(1)
		if j.down.Load() {
			http.Error(w, "down", 500)
			return
		}
		var out struct {
			Keys []map[string]string `json:"keys"`
		}
		for kid, k := range j.keys.Load().(map[string]*rsa.PrivateKey) {
			out.Keys = append(out.Keys, map[string]string{"kid": kid, "kty": "RSA", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func (j *jwks) set(t *testing.T, kids ...string) {
	m := map[string]*rsa.PrivateKey{}
	for _, kid := range kids {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		m[kid] = k
	}
	j.keys.Store(m)
}

func (j *jwks) key(kid string) *rsa.PrivateKey {
	return j.keys.Load().(map[string]*rsa.PrivateKey)[kid]
}

func sign(t *testing.T, k *rsa.PrivateKey, hdr, claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	in := enc(hdr) + "." + enc(claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func good() map[string]any {
	return map[string]any{"aud": []string{aud}, "iss": "https://" + team,
		"exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "email": "a@b.c"}
}

func verifier(t *testing.T, j *jwks) *Verifier {
	v := New(team, aud, slog.New(slog.NewTextHandler(io.Discard, nil)))
	v.certsURL = j.srv.URL
	return v
}

func TestVerify(t *testing.T) {
	j := newJWKS(t, "k1")
	v := verifier(t, j)
	if err := v.fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	k := j.key("k1")
	hdr := map[string]any{"alg": "RS256", "kid": "k1"}
	with := func(key string, val any) map[string]any { c := good(); c[key] = val; return c }

	if err := v.Verify(sign(t, k, hdr, good())); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if err := v.Verify(sign(t, k, hdr, with("aud", aud))); err != nil {
		t.Fatalf("string aud: %v", err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	bad := map[string]string{
		"wrong aud":   sign(t, k, hdr, with("aud", []string{"other"})),
		"wrong iss":   sign(t, k, hdr, with("iss", "https://evil.cloudflareaccess.com")),
		"expired":     sign(t, k, hdr, with("exp", time.Now().Add(-time.Hour).Unix())),
		"no exp":      sign(t, k, hdr, with("exp", 0)),
		"future nbf":  sign(t, k, hdr, with("nbf", time.Now().Add(time.Hour).Unix())),
		"forged":      sign(t, other, hdr, good()),
		"alg HS256":   sign(t, k, map[string]any{"alg": "HS256", "kid": "k1"}, good()),
		"alg none":    strings.Join(strings.Split(sign(t, k, map[string]any{"alg": "none", "kid": "k1"}, good()), ".")[:2], ".") + ".",
		"unknown kid": sign(t, k, map[string]any{"alg": "RS256", "kid": "k9"}, good()),
		"garbage":     "a.b",
	}
	for name, tok := range bad {
		if err := v.Verify(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Tampered claims with the original signature.
	parts := strings.Split(sign(t, k, hdr, good()), ".")
	evil, _ := json.Marshal(with("email", "attacker@x"))
	parts[1] = base64.RawURLEncoding.EncodeToString(evil)
	if v.Verify(strings.Join(parts, ".")) == nil {
		t.Error("tampered claims accepted")
	}
}

func TestKeyRotationAndRefetchLimit(t *testing.T) {
	j := newJWKS(t, "old")
	v := verifier(t, j)
	now := time.Now()
	v.now = func() time.Time { return now }
	v.fetch(context.Background())
	j.set(t, "new")
	now = now.Add(2 * time.Minute)
	tok := sign(t, j.key("new"), map[string]any{"alg": "RS256", "kid": "new"}, good())
	if err := v.Verify(tok); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	hits := j.hits.Load()
	for i := 0; i < 20; i++ {
		v.Verify(sign(t, j.key("new"), map[string]any{"alg": "RS256", "kid": "made-up"}, good()))
	}
	if j.hits.Load() != hits {
		t.Fatalf("unknown kids refetched %d times within a minute", j.hits.Load()-hits)
	}
}

func TestMiddleware(t *testing.T) {
	j := newJWKS(t, "k1")
	j.down.Store(true)
	v := verifier(t, j)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	h := v.Middleware(ok)
	tok := sign(t, j.key("k1"), map[string]any{"alg": "RS256", "kid": "k1"}, good())
	do := func(path, token string) int {
		req := httptest.NewRequest("GET", path, nil)
		if token != "" {
			req.Header.Set(Header, token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// Keys not loaded yet (Cloudflare unreachable): everything is refused.
	if c := do("/api/nodes", tok); c != 403 {
		t.Fatalf("before keys loaded: %d", c)
	}
	j.down.Store(false)
	v.fetch(context.Background())
	for _, p := range []string{"/", "/static/app.js", "/api/nodes"} {
		if c := do(p, ""); c != 403 {
			t.Errorf("%s without token: %d", p, c)
		}
		if c := do(p, tok); c != 200 {
			t.Errorf("%s with token: %d", p, c)
		}
	}
}
