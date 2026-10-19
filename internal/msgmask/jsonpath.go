package msgmask

import (
	"errors"
	"strconv"
	"strings"
)

// A JSON path of a rule: "$" then steps ".key", "[*]" or "[N]". Nothing
// else is accepted: no recursive descent, no filter, no slice.
type stepKind int

const (
	stepKey stepKind = iota
	stepIndex
	stepAny
)

type step struct {
	kind  stepKind
	key   string
	index int
}

type jpath []step

// maxPathSteps bounds a rule's path.
const maxPathSteps = 32

var errPath = errors.New(`want a JSON path such as "$.a.b", "$.a[*].b" or "$.a[3]"`)

func parsePath(s string) (jpath, error) {
	rest, ok := strings.CutPrefix(s, "$")
	if !ok || rest == "" {
		return nil, errPath
	}
	var p jpath
	for rest != "" {
		if len(p) == maxPathSteps {
			return nil, errors.New("JSON path: too many steps")
		}
		switch rest[0] {
		case '.':
			end := 1
			for end < len(rest) && rest[end] != '.' && rest[end] != '[' {
				end++
			}
			key := rest[1:end]
			if key == "" || strings.ContainsAny(key, "]*\"' \t\r\n,=") {
				return nil, errPath
			}
			p = append(p, step{kind: stepKey, key: key})
			rest = rest[end:]
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, errPath
			}
			in := rest[1:end]
			if in == "*" {
				p = append(p, step{kind: stepAny})
			} else {
				n, err := strconv.Atoi(in)
				if err != nil || n < 0 || strconv.Itoa(n) != in {
					return nil, errPath
				}
				p = append(p, step{kind: stepIndex, index: n})
			}
			rest = rest[end+1:]
		default:
			return nil, errPath
		}
	}
	return p, nil
}

func (p jpath) String() string {
	var b strings.Builder
	b.WriteByte('$')
	for _, s := range p {
		switch s.kind {
		case stepKey:
			b.WriteByte('.')
			b.WriteString(s.key)
		case stepIndex:
			b.WriteString("[" + strconv.Itoa(s.index) + "]")
		case stepAny:
			b.WriteString("[*]")
		}
	}
	return b.String()
}

// covers reports whether the rule path p names the concrete path c or one
// of its ancestors: a rule on an object covers everything below it.
func (p jpath) covers(c []step) bool {
	if len(p) > len(c) {
		return false
	}
	for i, s := range p {
		switch s.kind {
		case stepKey:
			if c[i].kind != stepKey || c[i].key != s.key {
				return false
			}
		case stepIndex:
			if c[i].kind != stepIndex || c[i].index != s.index {
				return false
			}
		case stepAny:
			if c[i].kind != stepIndex {
				return false
			}
		}
	}
	return true
}

// general turns a concrete path into a rule path, every index a [*].
func general(c []step) jpath {
	out := make(jpath, len(c))
	for i, s := range c {
		if s.kind == stepIndex {
			s = step{kind: stepAny}
		}
		out[i] = s
	}
	return out
}

// validKey reports a key a proposed rule path can name.
func validKey(k string) bool {
	return k != "" && !strings.ContainsAny(k, ".[]*\"' \t\r\n,=") && len(k) <= 128
}
