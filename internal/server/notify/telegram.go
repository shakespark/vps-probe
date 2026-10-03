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
)

// Telegram's limit is 4096 characters; stay under it.
const telegramMaxLen = 4000

type telegram struct {
	token, chatID string
	base          string // https://api.telegram.org; tests point it elsewhere
	client        *http.Client
}

// NewTelegram returns a channel that sends through a Telegram bot.
func NewTelegram(name, token, chatID string, log *slog.Logger) *Channel {
	t := &telegram{token: token, chatID: chatID, base: "https://api.telegram.org", client: &http.Client{Timeout: httpTimeout}}
	return newChannel(name, t, telegramMaxLen, log)
}

func (t *telegram) send(ctx context.Context, text string) error {
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

func (t *telegram) redact(err error) error {
	if t.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<bot_token>"))
}
