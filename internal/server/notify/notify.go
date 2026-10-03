// Package notify delivers alert messages. The Telegram client only ever
// calls sendMessage: it never reads updates, so the bot cannot be used to
// send anything to the server. Webhooks likewise only send.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Notifier accepts a message for delivery without blocking.
type Notifier interface {
	Notify(text string)
}

// Log writes messages to the log; used when no channel is configured.
type Log struct{ Log *slog.Logger }

func (l Log) Notify(text string) {
	l.Log.Info("alert (no telegram or webhook configured, not sent)", "message", text)
}

const (
	// Telegram's limit is 4096 characters; stay under it.
	maxMessage = 4000
	queueCap   = 100
	minBackoff = 2 * time.Second
	maxBackoff = 5 * time.Minute
	httpTimout = 20 * time.Second
)

type Telegram struct {
	token, chatID string
	base          string // https://api.telegram.org; tests point it elsewhere
	client        *http.Client
	log           *slog.Logger
	queue         chan string
}

func NewTelegram(token, chatID string, log *slog.Logger) *Telegram {
	return &Telegram{
		token:  token,
		chatID: chatID,
		base:   "https://api.telegram.org",
		client: &http.Client{Timeout: httpTimout},
		log:    log,
		queue:  make(chan string, queueCap),
	}
}

// Notify splits text into Telegram-sized messages and queues them. When the
// queue is full (Telegram unreachable for a long time) new messages are
// dropped, since they will be stale by the time it recovers anyway.
func (t *Telegram) Notify(text string) {
	for _, part := range Split(text, maxMessage) {
		select {
		case t.queue <- part:
		default:
			t.log.Error("telegram: queue full, dropping message", "message", part)
		}
	}
}

// Run delivers queued messages in order, retrying each with exponential
// backoff until it is sent or rejected outright.
func (t *Telegram) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-t.queue:
			t.deliver(ctx, msg)
		}
	}
}

func (t *Telegram) deliver(ctx context.Context, msg string) {
	deliver(ctx, t.log, "telegram", t.Send, msg)
}

// deliver sends one message over a channel, retrying with exponential
// backoff until it is sent or rejected outright.
func deliver(ctx context.Context, log *slog.Logger, channel string, send func(context.Context, string) error, msg string) {
	backoff := minBackoff
	for {
		err := send(ctx, msg)
		if err == nil {
			return
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			log.Error(channel+": message rejected, not retrying (check its settings in the config)", "err", err)
			return
		}
		wait := backoff
		var rl *rateLimited
		if errors.As(err, &rl) {
			wait = rl.after
		}
		log.Warn(channel+": send failed, retrying", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

type rateLimited struct{ after time.Duration }

func (e *rateLimited) Error() string { return fmt.Sprintf("rate limited, retry after %v", e.after) }

// Send makes one attempt. Errors never contain the bot token.
func (t *Telegram) Send(ctx context.Context, text string) error {
	form := url.Values{
		"chat_id":                  {t.chatID},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/bot"+t.token+"/sendMessage",
		strings.NewReader(form.Encode()))
	if err != nil {
		return t.redact(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return t.redact(err) // *url.Error includes the URL, and with it the token
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	json.Unmarshal(body, &r)
	switch {
	case resp.StatusCode == http.StatusOK && r.OK:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return &rateLimited{after: time.Duration(max(r.Parameters.RetryAfter, 1)) * time.Second}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return &permanentError{msg: t.redact(fmt.Errorf("HTTP %d: %s", resp.StatusCode, r.Description)).Error()}
	default:
		return t.redact(fmt.Errorf("HTTP %d: %s", resp.StatusCode, r.Description))
	}
}

func (t *Telegram) redact(err error) error {
	if t.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<bot_token>"))
}

// Split breaks text into parts of at most limit characters, preferring line
// boundaries.
func Split(text string, limit int) []string {
	var parts []string
	var cur strings.Builder
	n := 0
	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, strings.TrimRight(cur.String(), "\n"))
			cur.Reset()
			n = 0
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		ln := utf8.RuneCountInString(line)
		if n+ln > limit {
			flush()
		}
		for ln > limit { // a single overlong line
			r := []rune(line)
			parts = append(parts, string(r[:limit]))
			line = string(r[limit:])
			ln -= limit
		}
		cur.WriteString(line)
		n += ln
	}
	flush()
	return parts
}
