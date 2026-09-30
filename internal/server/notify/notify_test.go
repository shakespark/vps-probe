package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const token = "123456:SECRET-token-value"

// logBuf is written by the sender goroutine while tests read it.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fakeTG struct {
	mu     sync.Mutex
	got    []url.Values
	status []int // per request; last one repeats
	paths  []string
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	f.got = append(f.got, r.PostForm)
	f.paths = append(f.paths, r.URL.Path)
	st := f.status[min(len(f.got)-1, len(f.status)-1)]
	f.mu.Unlock()
	w.WriteHeader(st)
	switch st {
	case 200:
		io.WriteString(w, `{"ok":true}`)
	case 429:
		io.WriteString(w, `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
	default:
		io.WriteString(w, `{"ok":false,"description":"Bad Request: chat not found"}`)
	}
}

func (f *fakeTG) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func client(t *testing.T, f *fakeTG, logs *logBuf) *Telegram {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tg := NewTelegram(token, "-1001234567890", slog.New(slog.NewTextHandler(logs, nil)))
	tg.base = srv.URL
	return tg
}

func TestSendPlainText(t *testing.T) {
	f := &fakeTG{status: []int{200}}
	tg := client(t, f, &logBuf{})
	msg := "🔴 <b>not html</b> & {rich|text}"
	if err := tg.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	g := f.got[0]
	if g.Get("text") != msg || g.Get("chat_id") != "-1001234567890" || g.Has("parse_mode") {
		t.Fatalf("form = %v", g)
	}
	if f.paths[0] != "/bot"+token+"/sendMessage" {
		t.Fatalf("path = %s", f.paths[0])
	}
}

func TestRetriesAndRedaction(t *testing.T) {
	var logs logBuf
	f := &fakeTG{status: []int{500, 429, 200}}
	tg := client(t, f, &logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tg.Run(ctx)
	tg.Notify("hello")
	deadline := time.Now().Add(10 * time.Second)
	for f.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if f.count() != 3 {
		t.Fatalf("attempts = %d", f.count())
	}

	// Network error: *url.Error carries the URL; the token must not leak.
	bad := NewTelegram(token, "1", slog.New(slog.NewTextHandler(&logs, nil)))
	bad.base = "http://127.0.0.1:1"
	err := bad.Send(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "<bot_token>") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("token in logs")
	}
}

func TestRejectedNotRetried(t *testing.T) {
	var logs logBuf
	f := &fakeTG{status: []int{400}}
	tg := client(t, f, &logs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tg.Run(ctx)
	tg.Notify("a")
	tg.Notify("b")
	time.Sleep(500 * time.Millisecond)
	if f.count() != 2 {
		t.Fatalf("attempts = %d, want one per message", f.count())
	}
	if !strings.Contains(logs.String(), "chat not found") {
		t.Fatalf("logs: %s", logs.String())
	}
}

func TestSplit(t *testing.T) {
	lines := make([]string, 300)
	for i := range lines {
		lines[i] = strings.Repeat("字", 20)
	}
	text := strings.Join(lines, "\n")
	parts := Split(text, 4000)
	if len(parts) != 2 || strings.Join(parts, "\n") != text {
		t.Fatalf("parts = %d", len(parts))
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 4000 {
			t.Fatal("part too long")
		}
	}
	long := Split(strings.Repeat("x", 9000), 4000)
	if len(long) != 3 || len(long[2]) != 1000 {
		t.Fatalf("long line: %d parts", len(long))
	}
	if got := Split("short", 4000); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short: %v", got)
	}
}
