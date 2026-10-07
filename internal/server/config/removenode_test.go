package config

import (
	"os"
	"strings"
	"testing"
)

func TestRemoveNode(t *testing.T) {
	// us is a flow item over two lines; the comment above it stays, and so
	// does everything else.
	out, err := RemoveNode([]byte(handWritten), "us")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(handWritten, "  - { id: us, token: "+tokA+",\n      ping: { addr: probe.example.com, exclude: [hk] } }\n", "", 1)
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	c, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if hk, ok := c.Node("hk"); !ok || len(c.Nodes) != 1 || hk.Token != tokB || len(hk.Ping.Extra) != 1 || len(c.Notify) != 1 {
		t.Fatalf("the rest of the config changed: %+v", c.Nodes)
	}

	// The last item: the commented-out node and the comment of the next key
	// below it stay.
	out, err = RemoveNode(out, "hk")
	if err != nil {
		t.Fatal(err)
	}
	if want := "nodes:\n  # the server itself\n  # - id: old\n  #   token: gone\n\n# Telegram: a dedicated bot\nnotify:\n"; !strings.Contains(string(out), want) {
		t.Fatalf("got:\n%s", out)
	}
	// No node is left, and add-node works on what remains.
	if c, err := Parse(out); err != nil || len(c.Nodes) != 0 {
		t.Fatalf("after removing every node: %v", err)
	}
	if _, err := AddNode(out, NewNode{ID: "x", Token: tokC}); err != nil {
		t.Fatalf("add-node after removing every node: %v", err)
	}
}

// What add-node inserted, remove-node takes out again, byte for byte.
func TestRemoveNodeUndoesAddNode(t *testing.T) {
	example, err := os.ReadFile("../../../deploy/server.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	first, err := AddNode(example, NewNode{ID: "first", Token: tokA})
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"hand written":  handWritten,
		"example":       string(first),
		"nodes last":    "nodes:\n  - {id: a, token: " + tokA + "}\n",
		"no newline":    "nodes:\n- id: a\n  token: " + tokA,
		"deep indent":   "nodes:\n      - id: a\n        token: " + tokA + "\ndb: /x.db\n",
		"comment below": "nodes:\n  - id: a\n    token: " + tokA + "\n  # more to come\n\n# storage\ndb: /x.db\n",
	} {
		added, err := AddNode([]byte(in), NewNode{ID: "jp-1", Name: "东京", Token: tokC, PingAddr: "jp.example.com"})
		if err != nil {
			t.Errorf("%s: add: %v", name, err)
			continue
		}
		out, err := RemoveNode(added, "jp-1")
		if err != nil {
			t.Errorf("%s: remove: %v", name, err)
			continue
		}
		// AddNode ends a file that had no final newline with one.
		if string(out) != in && string(out) != in+"\n" {
			t.Errorf("%s: got:\n%s\nwant:\n%s", name, out, in)
		}
	}
}

func TestRemoveNodeRejects(t *testing.T) {
	rules := "nodes:\n  - {id: a, token: " + tokA + "}\n  - {id: b, token: " + tokB + "}\n"
	for name, c := range map[string]struct{ in, id, want string }{
		"unknown id":     {handWritten, "jp", "not in the config"},
		"ping.exclude":   {handWritten, "hk", "nodes[us].ping.exclude"},
		"rule nodes":     {rules + "alerts:\n  - {name: x, metric: offline, for: 60s, nodes: [a, b]}\n", "b", "alerts[x].nodes"},
		"rule exclude":   {rules + "alerts:\n  - {name: x, metric: offline, for: 60s, exclude: [b]}\n", "b", "alerts[x].exclude"},
		"report nodes":   {rules + "reports:\n  - {type: period, nodes: [b]}\n", "b", "reports[period].nodes"},
		"flow list":      {"nodes: [{id: a, token: " + tokA + "}]\n", "a", "block-style"},
		"invalid config": {"nodes:\n  - {id: a, token: short}\n", "a", "token"},
	} {
		out, err := RemoveNode([]byte(c.in), c.id)
		if err == nil {
			t.Errorf("%s: accepted:\n%s", name, out)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, c.want)
		}
	}
}
