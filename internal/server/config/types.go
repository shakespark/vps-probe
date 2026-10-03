package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration accepts Go durations plus a "d" (day) suffix, e.g. "400d".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func ParseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// String drops zero trailing units: 5m, 1h, 90s rather than 5m0s, 1h0m0s.
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// Scope is the nodes a rule or report applies to: all of them (the
// default), all but some, or a list.
type Scope struct {
	Nodes   NodeSet  `yaml:"nodes"`
	Exclude []string `yaml:"exclude"` // with nodes: all
}

// Covers reports whether the scope includes node id.
func (s Scope) Covers(id string) bool { return s.Nodes.Has(id) && !slices.Contains(s.Exclude, id) }

func (s Scope) validate(p *problems, where string, ids map[string]bool) {
	for _, id := range s.Nodes.IDs {
		if !ids[id] {
			p.add("%s: nodes: %q is not a configured node", where, id)
		}
	}
	if s.Exclude == nil {
		return
	}
	if s.Nodes.IDs != nil {
		p.add("%s: exclude only goes with nodes: all; drop the node from the nodes list instead", where)
	}
	if len(s.Exclude) == 0 {
		p.add("%s: exclude: empty list; omit it", where)
	}
	for _, id := range s.Exclude {
		if !ids[id] {
			p.add("%s: exclude: %q is not a configured node", where, id)
		}
	}
}

// NodeSet is either "all" (the default) or a list of node ids.
type NodeSet struct {
	IDs []string // nil means all
}

func (n *NodeSet) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind == yaml.ScalarNode {
		if v.Value != "all" {
			return fmt.Errorf("nodes: want \"all\" or a list of node ids, got %q", v.Value)
		}
		n.IDs = nil
		return nil
	}
	var ids []string
	if err := v.Decode(&ids); err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("nodes: empty list; omit it or write \"all\"")
	}
	n.IDs = ids
	return nil
}

func (n NodeSet) Has(id string) bool { return n.IDs == nil || slices.Contains(n.IDs, id) }

// decodeStrict decodes a mapping into the struct v points to and, unlike
// Node.Decode, rejects keys v has no field for: a typo must not silently
// become a default. Types with their own UnmarshalYAML use it.
func decodeStrict(n *yaml.Node, v any) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: want a mapping", n.Line)
	}
	known := map[string]bool{}
	var collect func(t reflect.Type)
	collect = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if f.Anonymous && tag == "" {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				collect(ft)
			} else if tag != "" && tag != "-" {
				known[tag] = true
			}
		}
	}
	collect(reflect.TypeOf(v).Elem())
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; !known[k.Value] {
			return fmt.Errorf("line %d: field %s not found", k.Line, k.Value)
		}
	}
	return n.Decode(v)
}
