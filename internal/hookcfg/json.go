package hookcfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// A small order-preserving JSON tree. Leaves (strings, numbers, true,
// false, null) keep their raw text, so untouched values round-trip
// byte for byte. Containers are rewritten with 2-space indentation.

type kind int

const (
	leaf kind = iota
	object
	array
)

type member struct {
	raw  string // key as written, with quotes
	name string // decoded key
	val  *node
}

type node struct {
	kind    kind
	raw     string   // leaf
	members []member // object
	items   []*node  // array
}

// parseTree parses valid JSON. The caller checks validity first.
func parseTree(data []byte) (*node, error) {
	if !json.Valid(data) {
		return nil, fmt.Errorf("not valid JSON")
	}
	p := &parser{b: data}
	n := p.value()
	return n, nil
}

type parser struct {
	b   []byte
	pos int
}

func (p *parser) ws() {
	for p.pos < len(p.b) {
		switch p.b[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) str() string {
	start := p.pos
	p.pos++ // opening quote
	for p.b[p.pos] != '"' {
		if p.b[p.pos] == '\\' {
			p.pos++
		}
		p.pos++
	}
	p.pos++
	return string(p.b[start:p.pos])
}

func (p *parser) value() *node {
	p.ws()
	switch p.b[p.pos] {
	case '{':
		p.pos++
		n := &node{kind: object}
		for {
			p.ws()
			if p.b[p.pos] == '}' {
				p.pos++
				return n
			}
			if p.b[p.pos] == ',' {
				p.pos++
				continue
			}
			raw := p.str()
			var name string
			json.Unmarshal([]byte(raw), &name)
			p.ws()
			p.pos++ // colon
			n.members = append(n.members, member{raw: raw, name: name, val: p.value()})
		}
	case '[':
		p.pos++
		n := &node{kind: array}
		for {
			p.ws()
			if p.b[p.pos] == ']' {
				p.pos++
				return n
			}
			if p.b[p.pos] == ',' {
				p.pos++
				continue
			}
			n.items = append(n.items, p.value())
		}
	case '"':
		return &node{kind: leaf, raw: p.str()}
	}
	start := p.pos
	for p.pos < len(p.b) && !strings.ContainsRune(",]} \t\r\n", rune(p.b[p.pos])) {
		p.pos++
	}
	return &node{kind: leaf, raw: string(p.b[start:p.pos])}
}

// write renders n. With pretty set it uses 2-space indentation, like
// JSON.stringify(x, null, 2). Otherwise the output is compact.
func (n *node) write(buf *bytes.Buffer, pretty bool, depth int) {
	nl := func(d int) {
		if pretty {
			buf.WriteByte('\n')
			buf.WriteString(strings.Repeat("  ", d))
		}
	}
	switch n.kind {
	case leaf:
		buf.WriteString(n.raw)
	case object:
		if len(n.members) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteByte('{')
		for i, m := range n.members {
			if i > 0 {
				buf.WriteByte(',')
			}
			nl(depth + 1)
			buf.WriteString(m.raw)
			buf.WriteByte(':')
			if pretty {
				buf.WriteByte(' ')
			}
			m.val.write(buf, pretty, depth+1)
		}
		nl(depth)
		buf.WriteByte('}')
	case array:
		if len(n.items) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteByte('[')
		for i, it := range n.items {
			if i > 0 {
				buf.WriteByte(',')
			}
			nl(depth + 1)
			it.write(buf, pretty, depth+1)
		}
		nl(depth)
		buf.WriteByte(']')
	}
}

func (n *node) compact() string {
	var b bytes.Buffer
	n.write(&b, false, 0)
	return b.String()
}

// get returns the value of the first member called name, or nil.
func (n *node) get(name string) *node {
	if n.kind != object {
		return nil
	}
	for _, m := range n.members {
		if m.name == name {
			return m.val
		}
	}
	return nil
}

func (n *node) set(name string, v *node) {
	raw, _ := json.Marshal(name)
	n.members = append(n.members, member{raw: string(raw), name: name, val: v})
}

func (n *node) remove(name string) {
	for i, m := range n.members {
		if m.name == name {
			n.members = append(n.members[:i], n.members[i+1:]...)
			return
		}
	}
}

// str returns the decoded string value of a string leaf.
func (n *node) str() (string, bool) {
	if n == nil || n.kind != leaf || !strings.HasPrefix(n.raw, `"`) {
		return "", false
	}
	var s string
	if json.Unmarshal([]byte(n.raw), &s) != nil {
		return "", false
	}
	return s, true
}

// fromGo builds a node from a Go value. Struct field order is kept.
func fromGo(v any) *node {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // only fixed internal shapes go in
	}
	n, err := parseTree(bytes.TrimSpace(buf.Bytes()))
	if err != nil {
		panic(err)
	}
	return n
}
