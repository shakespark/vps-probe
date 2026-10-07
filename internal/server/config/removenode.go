package config

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// refs lists where the config names node id outside its own entry.
func (c *Config) refs(id string) []string {
	var out []string
	for _, n := range c.Nodes {
		if n.ID != id && slices.Contains(n.Ping.Exclude, id) {
			out = append(out, "nodes["+n.ID+"].ping.exclude")
		}
	}
	scope := func(where string, s Scope) {
		if slices.Contains(s.Nodes.IDs, id) {
			out = append(out, where+".nodes")
		}
		if slices.Contains(s.Exclude, id) {
			out = append(out, where+".exclude")
		}
	}
	for _, r := range c.Alerts {
		scope("alerts["+r.Name+"]", r.Scope)
	}
	for _, r := range c.Reports {
		scope("reports["+r.Type+"]", r.Scope)
	}
	return out
}

// RemoveNode returns data without node id's item in the nodes list. Like
// AddNode it edits the text: the item's lines are cut out and every other
// byte stays as it was, including comments above the item. Places that
// name the node (another node's ping.exclude, a rule's nodes or exclude)
// are not edited: while there are any, the input is rejected. The result
// must be the old config minus exactly this node.
func RemoveNode(data []byte, id string) ([]byte, error) {
	old, err := Parse(data)
	if err != nil {
		return nil, err
	}
	idx := slices.IndexFunc(old.Nodes, func(n Node) bool { return n.ID == id })
	if idx < 0 {
		return nil, fmt.Errorf("node %q is not in the config", id)
	}
	if refs := old.refs(id); len(refs) > 0 {
		return nil, fmt.Errorf("node %q is still named in %s; take it out there first, then run remove-node again",
			id, strings.Join(refs, ", "))
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	byHand := errors.New("config: cannot tell where this node's lines begin and end; remove it by hand")
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, byHand
	}
	root := doc.Content[0]
	var seq *yaml.Node
	nextKeyLine := 0 // line of the top-level key after nodes; 0 = nodes is last
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "nodes" {
			seq = root.Content[i+1]
			if i+2 < len(root.Content) {
				nextKeyLine = root.Content[i+2].Line
			}
			break
		}
	}
	if seq == nil || seq.Kind != yaml.SequenceNode || len(seq.Content) != len(old.Nodes) {
		return nil, byHand
	}
	if seq.Style&yaml.FlowStyle != 0 {
		return nil, errors.New("config: remove-node needs a block-style nodes list (\"nodes:\" followed by \"- ...\" lines); remove this node by hand")
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// The item starts on the line of its "- " and ends before the next
	// item, the next top-level key or the end of the file.
	item := seq.Content[idx]
	from, to := item.Line-1, len(lines)
	if idx+1 < len(seq.Content) {
		to = seq.Content[idx+1].Line - 1
	} else if nextKeyLine > 0 {
		to = nextKeyLine - 1
	}
	if dash := item.Column - 3; dash < 0 || from >= to || to > len(lines) ||
		!strings.HasPrefix(lines[from], strings.Repeat(" ", dash)+"- ") {
		return nil, byHand
	}
	// Blank lines and comments below the item introduce what follows.
	for to > from+1 {
		if l := strings.TrimSpace(lines[to-1]); l != "" && !strings.HasPrefix(l, "#") {
			break
		}
		to--
	}
	var out bytes.Buffer
	for _, l := range lines[:from] {
		out.WriteString(l)
	}
	for _, l := range lines[to:] {
		out.WriteString(l)
	}

	c, err := Parse(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("the config would not be valid without this node: %w", err)
	}
	rest := slices.Delete(slices.Clone(old.Nodes), idx, idx+1)
	if !slices.EqualFunc(c.Nodes, rest, func(a, b Node) bool { return reflect.DeepEqual(a, b) }) {
		return nil, errors.New("config: cutting the node out would change another one; remove this node by hand")
	}
	old.Nodes, c.Nodes = nil, nil
	if !reflect.DeepEqual(old, c) {
		return nil, errors.New("config: cutting the node out would change other settings; remove this node by hand")
	}
	return out.Bytes(), nil
}
