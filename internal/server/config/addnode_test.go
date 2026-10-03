package config

import (
	"strings"
	"testing"
)

const tokC = "cdefghijklmnopqrstuvwxyz0123456789ab"

// A hand-written file: comments everywhere, flow and block items, a
// commented-out node, and keys after the list.
const handWritten = `# my probe
listen:
  web: 127.0.0.1:8080   # for cloudflared
timezone: Asia/Shanghai

nodes:
  # the server itself
  - { id: us, token: ` + tokA + `, addr: probe.example.com,
      no_ping: [hk] }
  - id: hk
    name: "香港"   # a long description that a YAML encoder would happily rewrap or requote if the file were re-encoded
    token: ` + tokB + `
    extra_peers:
      - { name: cf, addr: 1.1.1.1 }
  # - id: old
  #   token: gone

# Telegram: a dedicated bot
telegram:
  bot_token: "1:a"
  chat_id: 5
`

func TestAddNode(t *testing.T) {
	n := NewNode{ID: "jp-1", Name: "东京: 1", Token: tokC, Addr: "jp.example.com", Region: "JP"}
	out, err := AddNode([]byte(handWritten), n)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one block was inserted, after the commented-out node and
	// before the blank line and comment that introduce the next key.
	want := strings.Replace(handWritten, "  #   token: gone\n", "  #   token: gone\n"+
		"  - id: jp-1\n    name: '东京: 1'\n    token: "+tokC+"\n    addr: jp.example.com\n    region: JP\n", 1)
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	c, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Node("jp-1")
	if !ok || len(c.Nodes) != 3 || got.Name != "东京: 1" || got.Token != tokC || got.Addr != "jp.example.com" || got.Region != "JP" {
		t.Fatalf("node: %+v", got)
	}
	if !c.Telegram.Enabled() || !c.NoPing("us", "hk") {
		t.Fatal("the rest of the config changed")
	}

	// A second one goes after the first.
	out2, err := AddNode(out, NewNode{ID: "sg", Token: tokA[1:] + "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), "    region: JP\n  - id: sg\n    token: ") {
		t.Fatalf("second insert:\n%s", out2)
	}
}

func TestAddNodeLayouts(t *testing.T) {
	item := NewNode{ID: "new", Token: tokC}
	for name, c := range map[string]struct{ in, want string }{
		"nodes last, no final newline": {
			"timezone: UTC\nnodes:\n- id: a\n  token: " + tokA,
			"timezone: UTC\nnodes:\n- id: a\n  token: " + tokA + "\n- id: new\n  token: " + tokC + "\n",
		},
		"nodes last, trailing comment": {
			"nodes:\n    - {id: a, token: " + tokA + "}\n\n# the end\n",
			"nodes:\n    - {id: a, token: " + tokA + "}\n    - id: new\n      token: " + tokC + "\n\n# the end\n",
		},
		"key right after": {
			"nodes:\n  - {id: a, token: " + tokA + "}\ntimezone: UTC\n",
			"nodes:\n  - {id: a, token: " + tokA + "}\n  - id: new\n    token: " + tokC + "\ntimezone: UTC\n",
		},
	} {
		out, err := AddNode([]byte(c.in), item)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if string(out) != c.want {
			t.Errorf("%s: got\n%s\nwant\n%s", name, out, c.want)
		}
	}
}

func TestAddNodeRejects(t *testing.T) {
	for name, c := range map[string]struct {
		in string
		n  NewNode
	}{
		"duplicate id":    {handWritten, NewNode{ID: "hk", Token: tokC}},
		"duplicate token": {handWritten, NewNode{ID: "x", Token: tokA}},
		"bad id":          {handWritten, NewNode{ID: "a b", Token: tokC}},
		"short token":     {handWritten, NewNode{ID: "x", Token: "short"}},
		"bad addr":        {handWritten, NewNode{ID: "x", Token: tokC, Addr: "not a host"}},
		"flow list":       {"nodes: [{id: a, token: " + tokA + "}]\n", NewNode{ID: "x", Token: tokC}},
		"invalid config":  {"nodes:\n  - {id: a, token: short}\n", NewNode{ID: "x", Token: tokC}},
	} {
		if out, err := AddNode([]byte(c.in), c.n); err == nil {
			t.Errorf("%s: accepted:\n%s", name, out)
		}
	}
}
