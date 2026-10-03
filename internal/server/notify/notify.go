// Package notify delivers alert messages. Channels only send: the Telegram
// client only ever calls sendMessage and never reads updates, and a
// webhook's reply is never used, so no channel can be used to send anything
// to the server.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shakespark/vps-probe/internal/server/config"
)

// Notifier accepts a message for delivery without blocking.
type Notifier interface {
	Notify(text string)
}

// Log writes messages to the log; used when no channel is configured.
type Log struct{ Log *slog.Logger }

func (l Log) Notify(text string) {
	l.Log.Info("alert (no notify channel configured, not sent)", "message", text)
}

// Multi sends every message to several channels.
type Multi []Notifier

func (m Multi) Notify(text string) {
	for _, n := range m {
		n.Notify(text)
	}
}

const (
	queueCap    = 100
	minBackoff  = 2 * time.Second
	maxBackoff  = 5 * time.Minute
	httpTimeout = 20 * time.Second
)

// sender makes one attempt to deliver a message. A *permanentError means
// retrying is pointless, a *rateLimited says when to retry. Errors must not
// contain the channel's secrets: they are logged.
type sender interface {
	send(ctx context.Context, text string) error
}

// Channel is one destination: a queue in front of a sender, delivered in
// order by Run with retries.
type Channel struct {
	Name   string
	sender sender
	maxLen int // messages longer than this many characters are split; 0 = never
	log    *slog.Logger
	queue  chan string
}

// New returns the channel a config entry describes.
func New(c config.Channel, log *slog.Logger) *Channel {
	if c.Type == config.Telegram {
		return NewTelegram(c.Name, c.BotToken, c.ChatID, log)
	}
	return NewWebhook(c.Name, c.Method, c.URL, c.Headers, c.Body, log)
}

func newChannel(name string, s sender, maxLen int, log *slog.Logger) *Channel {
	return &Channel{Name: name, sender: s, maxLen: maxLen, log: log, queue: make(chan string, queueCap)}
}

// Notify queues text. When the queue is full (the channel has been
// unreachable for a long time) new messages are dropped, since they would be
// stale by the time it recovers anyway.
func (c *Channel) Notify(text string) {
	for _, part := range c.split(text) {
		select {
		case c.queue <- part:
		default:
			c.log.Error(c.Name+": queue full, dropping message", "message", part)
		}
	}
}

func (c *Channel) split(text string) []string {
	if c.maxLen == 0 {
		return []string{text}
	}
	return Split(text, c.maxLen)
}

// Send makes one attempt to deliver text now, bypassing the queue.
func (c *Channel) Send(ctx context.Context, text string) error {
	for _, part := range c.split(text) {
		if err := c.sender.send(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

// Run delivers queued messages in order until ctx is done, retrying each
// with exponential backoff until it is sent or rejected outright.
func (c *Channel) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.queue:
			c.deliver(ctx, msg)
		}
	}
}

func (c *Channel) deliver(ctx context.Context, msg string) {
	backoff := minBackoff
	for {
		err := c.sender.send(ctx, msg)
		if err == nil {
			return
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			c.log.Error(c.Name+": message rejected, not retrying (check the channel's settings in the config)", "err", err)
			return
		}
		wait := backoff
		var rl *rateLimited
		if errors.As(err, &rl) {
			wait = rl.after
		}
		c.log.Warn(c.Name+": send failed, retrying", "err", err, "in", wait)
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
