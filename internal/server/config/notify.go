package config

import (
	"net/url"
	"slices"
	"strings"

	"github.com/shakespark/vps-probe/internal/wire"
)

// Channel types.
const (
	Telegram = "telegram"
	Webhook  = "webhook"
)

// Channel is one place alert messages are sent to. Channels only send:
// nothing is ever read back from them. With no channel, alerts are
// evaluated and recorded but not sent.
type Channel struct {
	Type string `yaml:"type"`
	// Name labels the channel in logs and on the alerts page. A telegram
	// channel is called "telegram" when it has none.
	Name string `yaml:"name"`

	// telegram: the bot only ever calls sendMessage.
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"` // a group id like -100123 may be written unquoted

	// webhook: one HTTP request per message, built from a template; see
	// notify.Webhook for {{title}} and {{message}}.
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"` // POST (default), GET or PUT
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
}

// ChannelNames lists where alerts are sent, for display.
func (c *Config) ChannelNames() []string {
	out := make([]string, len(c.Notify))
	for i, ch := range c.Notify {
		out[i] = ch.Name
	}
	return out
}

func (c *Config) validateNotify(p *problems) {
	names := map[string]bool{}
	for i := range c.Notify {
		ch := &c.Notify[i]
		if ch.Type == Telegram && ch.Name == "" {
			ch.Name = Telegram
		}
		where := "notify[" + ch.Name + "]"
		switch {
		case !wire.ValidID(ch.Name):
			p.add("notify[%d].name %q: want letters, digits, '.', '_', '-' (it names the channel in logs and on the alerts page)", i, ch.Name)
		case names[ch.Name]:
			p.add("%s: name used twice", where)
		}
		names[ch.Name] = true
		switch ch.Type {
		case Telegram:
			if ch.BotToken == "" || ch.ChatID == "" {
				p.add("%s: a telegram channel needs bot_token and chat_id", where)
			}
			if ch.URL != "" || ch.Method != "" || ch.Headers != nil || ch.Body != "" {
				p.add("%s: url, method, headers and body belong to webhook channels", where)
			}
		case Webhook:
			ch.validateWebhook(p, where)
		default:
			p.add("notify[%d].type %q: want telegram or webhook", i, ch.Type)
		}
	}
}

func (ch *Channel) validateWebhook(p *problems, where string) {
	if ch.BotToken != "" || ch.ChatID != "" {
		p.add("%s: bot_token and chat_id belong to telegram channels", where)
	}
	ch.Method = strings.ToUpper(ch.Method)
	if ch.Method == "" {
		ch.Method = "POST"
	}
	if !slices.Contains([]string{"GET", "POST", "PUT"}, ch.Method) {
		p.add("%s.method %q: want GET, POST or PUT", where, ch.Method)
	}
	if ch.Method == "GET" && ch.Body != "" {
		p.add("%s: a GET request has no body; put {{title}} / {{message}} in the url", where)
	}
	// The placeholders stand for encoded text, which cannot break a URL.
	u, err := url.Parse(strings.NewReplacer("{{title}}", "t", "{{message}}", "m").Replace(ch.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.add("%s.url: want an http(s) URL", where)
	}
	for k, v := range ch.Headers {
		if k == "" || strings.ContainsAny(k, " :\r\n") || strings.ContainsAny(v, "\r\n") {
			p.add("%s.headers: bad header %q", where, k)
		}
	}
}
