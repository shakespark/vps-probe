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

// webhook sends each message as one HTTP request built from a template, so
// any push service with an HTTP API can be a channel (Bark, ntfy, Discord,
// Slack, Server酱, ...). The reply's status decides whether to retry, and
// its body is never used.
//
// {{title}} is the message's first line, {{message}} the whole text. In
// the URL they are percent-encoded; in the body they are encoded for the
// body's content type: a JSON string literal (with its quotes) for JSON,
// percent-encoded for a form, as is for anything else.
type webhook struct {
	method, url, body string
	headers           map[string]string
	client            *http.Client
}

const (
	TitleVar   = "{{title}}"
	MessageVar = "{{message}}"
	// DefaultWebhookBody is sent when a POST webhook has no body template.
	DefaultWebhookBody = `{"title": {{title}}, "message": {{message}}}`
)

// NewWebhook returns a channel that sends HTTP requests. method defaults to
// POST, and the body of a request that has one to DefaultWebhookBody.
func NewWebhook(name, method, rawURL string, headers map[string]string, body string, log *slog.Logger) *Channel {
	if method == "" {
		method = http.MethodPost
	}
	if body == "" && method != http.MethodGet {
		body = DefaultWebhookBody
	}
	h := map[string]string{}
	for k, v := range headers {
		h[http.CanonicalHeaderKey(k)] = v
	}
	if _, ok := h["Content-Type"]; !ok && body != "" {
		h["Content-Type"] = "application/json"
	}
	w := &webhook{method: method, url: rawURL, body: body, headers: h, client: &http.Client{Timeout: httpTimeout}}
	return newChannel(name, w, 0, log)
}

func escapeURL(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

func fill(tmpl string, enc func(string) string, text string) string {
	title, _, _ := strings.Cut(text, "\n")
	return strings.NewReplacer(TitleVar, enc(title), MessageVar, enc(text)).Replace(tmpl)
}

// send makes one attempt. The URL usually carries a key, so errors never
// include it.
func (w *webhook) send(ctx context.Context, text string) error {
	enc := func(s string) string { return s }
	switch ct := strings.ToLower(w.headers["Content-Type"]); {
	case strings.Contains(ct, "json"):
		enc = func(s string) string {
			b, _ := json.Marshal(s)
			return string(b)
		}
	case strings.Contains(ct, "x-www-form-urlencoded"):
		enc = escapeURL
	}
	var body io.Reader
	if w.body != "" {
		body = strings.NewReader(fill(w.body, enc, text))
	}
	req, err := http.NewRequestWithContext(ctx, w.method, fill(w.url, escapeURL, text), body)
	if err != nil {
		return &permanentError{msg: "the URL is not valid once the message is filled in"}
	}
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		// *url.Error includes the URL; keep only what went wrong.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return &rateLimited{after: time.Minute}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return &permanentError{msg: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}
