package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const alertText = "🔴 告警 cpu_high · 香港 \"1\"\nCPU 97.3% & rising\n2026-10-03 12:00:00"

type hit struct {
	method, uri, ctype, auth, body string
}

type hookServer struct {
	mu     sync.Mutex
	hits   []hit
	status []int // per request; the last one repeats
	*httptest.Server
}

func newHookServer(status ...int) *hookServer {
	s := &hookServer{status: status}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.hits = append(s.hits, hit{r.Method, r.RequestURI, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(b)})
		code := 200
		if len(s.status) > 0 {
			code = s.status[min(len(s.hits), len(s.status))-1]
		}
		s.mu.Unlock()
		w.WriteHeader(code)
		io.WriteString(w, "a reply nobody reads")
	}))
	return s
}

func (s *hookServer) got() []hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]hit(nil), s.hits...)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestWebhookTemplates(t *testing.T) {
	s := newHookServer()
	defer s.Close()
	title := "🔴 告警 cpu_high · 香港 \"1\""
	for name, c := range map[string]struct {
		method, path, body string
		headers            map[string]string
		want               hit
	}{
		"default body is JSON": {
			want: hit{"POST", "/", "application/json", "",
				`{"title": "🔴 告警 cpu_high · 香港 \"1\"", "message": "🔴 告警 cpu_high · 香港 \"1\"\nCPU 97.3% \u0026 rising\n2026-10-03 12:00:00"}`},
		},
		"custom JSON body and header": {
			body: `{"content": {{message}}}`, headers: map[string]string{"authorization": "Bearer k"},
			want: hit{"POST", "/", "application/json", "Bearer k",
				`{"content": "🔴 告警 cpu_high · 香港 \"1\"\nCPU 97.3% \u0026 rising\n2026-10-03 12:00:00"}`},
		},
		"form body": {
			body: "title={{title}}&desp={{message}}", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			want: hit{"POST", "/", "application/x-www-form-urlencoded", "",
				"title=" + escapeURL(title) + "&desp=" + escapeURL(alertText)},
		},
		"plain text body": {
			body: "{{message}}", headers: map[string]string{"Content-Type": "text/plain"},
			want: hit{"POST", "/", "text/plain", "", alertText},
		},
		"GET with the text in the path and query": {
			method: "GET", path: "/key/{{title}}?body={{message}}",
			want: hit{"GET", "/key/" + escapeURL(title) + "?body=" + escapeURL(alertText), "", "", ""},
		},
	} {
		before := len(s.got())
		w := NewWebhook("x", c.method, s.URL+c.path, c.headers, c.body, quiet())
		if c.path == "" {
			w = NewWebhook("x", c.method, s.URL+"/", c.headers, c.body, quiet())
		}
		if err := w.Send(context.Background(), alertText); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := s.got()[before]; got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, c.want)
		}
	}
	// The encoded text cannot add a path segment or a query parameter.
	if e := escapeURL("a/b?c=d&e f+g"); strings.ContainsAny(e, "/?&= +") {
		t.Fatalf("escapeURL left a URL metacharacter: %s", e)
	}
}

func TestWebhookStatusAndSecrets(t *testing.T) {
	for _, c := range []struct {
		status    int
		ok, final bool // final: must not be retried
	}{{200, true, false}, {204, true, false}, {400, false, true}, {404, false, true}, {500, false, false}, {429, false, false}} {
		s := newHookServer(c.status)
		err := NewWebhook("bark", "", s.URL+"/SECRET-KEY", nil, "", quiet()).Send(context.Background(), alertText)
		s.Close()
		if (err == nil) != c.ok {
			t.Errorf("HTTP %d: err = %v", c.status, err)
		}
		if _, perm := err.(*permanentError); perm != c.final {
			t.Errorf("HTTP %d: permanent = %v, want %v", c.status, perm, c.final)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET-KEY") {
			t.Errorf("HTTP %d: the error leaks the URL: %v", c.status, err)
		}
	}
	// A server that is not there: the error must still not name the URL.
	err := NewWebhook("bark", "", "http://127.0.0.1:1/SECRET-KEY", nil, "", quiet()).Send(context.Background(), alertText)
	if err == nil || strings.Contains(err.Error(), "SECRET-KEY") {
		t.Fatalf("unreachable server: %v", err)
	}
}

func TestWebhookQueueAndMulti(t *testing.T) {
	a, b := newHookServer(), newHookServer(403)
	defer a.Close()
	defer b.Close()
	wa := NewWebhook("a", "", a.URL, nil, "", quiet())
	wb := NewWebhook("b", "", b.URL, nil, "", quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wa.Run(ctx)
	go wb.Run(ctx)
	m := Multi{wa, wb}
	m.Notify("one")
	m.Notify("two")
	deadline := time.Now().Add(5 * time.Second)
	for (len(a.got()) < 2 || len(b.got()) < 2) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Both channels got both messages in order; b's rejections were not retried.
	if got := a.got(); len(got) != 2 || !strings.Contains(got[0].body, `"one"`) || !strings.Contains(got[1].body, `"two"`) {
		t.Fatalf("a: %+v", got)
	}
	time.Sleep(50 * time.Millisecond)
	if got := b.got(); len(got) != 2 {
		t.Fatalf("b: %d requests, want 2 (a 403 is final)", len(got))
	}
}
