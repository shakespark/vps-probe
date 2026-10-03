package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// NewNode is what add-node writes; everything else about a node is edited
// by hand.
type NewNode struct {
	ID, Name, Token, Region, Group string
	PingAddr                       string // where other nodes ping it; none = they don't
}

// yaml returns the node as a list item's content, block style.
func (n NewNode) yaml() ([]byte, error) {
	type ping struct {
		Addr string `yaml:"addr"`
	}
	item := struct {
		ID     string `yaml:"id"`
		Name   string `yaml:"name,omitempty"`
		Token  string `yaml:"token"`
		Region string `yaml:"region,omitempty"`
		Group  string `yaml:"group,omitempty"`
		Ping   *ping  `yaml:"ping,omitempty"`
	}{ID: n.ID, Name: n.Name, Token: n.Token, Region: n.Region, Group: n.Group}
	if n.PingAddr != "" {
		item.Ping = &ping{n.PingAddr}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	err := enc.Encode(item)
	return buf.Bytes(), err
}

// AddNode returns data with n appended to the nodes list. The file is hand
// written, so it is not re-encoded: the YAML parser only finds where the
// list ends and how it is indented, and the new item is inserted as text.
// Every other byte stays as it was. The result must pass Parse, or the
// input is rejected.
func AddNode(data []byte, n NewNode) ([]byte, error) {
	old, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if _, dup := old.Node(n.ID); dup {
		return nil, fmt.Errorf("node %q is already in the config", n.ID)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config: not a YAML mapping")
	}
	root := doc.Content[0]
	var key, seq *yaml.Node
	nextKeyLine := 0 // line of the top-level key after nodes; 0 = nodes is last
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "nodes" {
			key, seq = root.Content[i], root.Content[i+1]
			if i+2 < len(root.Content) {
				nextKeyLine = root.Content[i+2].Line
			}
			break
		}
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	indent := 2
	switch {
	case seq == nil:
		return nil, errors.New("config: no nodes key; add \"nodes: []\" and run add-node again")
	case len(old.Nodes) == 0:
		// The first node: "nodes: []" (or an empty "nodes:") on a line of
		// its own becomes the head of a block list. A comment on that line
		// is kept.
		head := lines[key.Line-1]
		rest, ok := strings.CutPrefix(strings.TrimRight(head, "\r\n"), "nodes:")
		value, comment, commented := strings.Cut(rest, "#")
		if v := strings.TrimSpace(value); !ok || v != "" && v != "[]" {
			return nil, errors.New("config: cannot add the first node to this nodes list; write \"nodes: []\" on a line of its own")
		}
		lines[key.Line-1] = "nodes:\n"
		if commented {
			lines[key.Line-1] = "nodes:   #" + comment + "\n"
		}
		nextKeyLine = key.Line + 1 // the item goes right below the key
		if nextKeyLine > len(lines) {
			nextKeyLine = 0
		}
	case seq.Kind != yaml.SequenceNode || seq.Style&yaml.FlowStyle != 0:
		return nil, errors.New("config: add-node needs a block-style nodes list (\"nodes:\" followed by \"- ...\" lines); add this node by hand")
	default:
		// An item's column is where its content starts, after "- ".
		if indent = seq.Content[0].Column - 3; indent < 0 {
			return nil, errors.New("config: cannot tell how the nodes list is indented; add this node by hand")
		}
	}

	item, err := n.yaml()
	if err != nil {
		return nil, err
	}
	var block strings.Builder
	pad := strings.Repeat(" ", indent)
	for i, l := range strings.Split(strings.TrimRight(string(item), "\n"), "\n") {
		lead := "  "
		if i == 0 {
			lead = "- "
		}
		block.WriteString(pad + lead + l + "\n")
	}

	at := len(lines) // index to insert before
	if nextKeyLine > 0 {
		at = nextKeyLine - 1
	}
	// Blank lines and top-level comments above the next key belong to it.
	for at > 0 && len(old.Nodes) > 0 {
		l := lines[at-1]
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") {
			break
		}
		at--
	}
	var out bytes.Buffer
	for _, l := range lines[:at] {
		out.WriteString(l)
	}
	if at > 0 && !strings.HasSuffix(lines[at-1], "\n") {
		out.WriteString("\n")
	}
	out.WriteString(block.String())
	for _, l := range lines[at:] {
		out.WriteString(l)
	}

	// Whatever the file looked like, the result must be the old config plus
	// exactly this node at the end of the list.
	c, err := Parse(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("the config would not be valid with this node: %w", err)
	}
	if len(c.Nodes) != len(old.Nodes)+1 {
		return nil, errors.New("config: could not find where the nodes list ends; add this node by hand")
	}
	last := c.Nodes[len(c.Nodes)-1]
	if last.ID != n.ID || last.Token != n.Token || last.Ping.Addr != n.PingAddr {
		return nil, errors.New("config: could not find where the nodes list ends; add this node by hand")
	}
	for i := range old.Nodes {
		if c.Nodes[i].ID != old.Nodes[i].ID || c.Nodes[i].Token != old.Nodes[i].Token {
			return nil, errors.New("config: inserting the node would change another one; add this node by hand")
		}
	}
	return out.Bytes(), nil
}
