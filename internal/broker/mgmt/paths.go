package mgmt

import (
	"errors"
	"net/url"
	"strings"
)

// Path names one allowlisted endpoint: Kind is a key of Allowed, Segs fill
// its {v} (vhost) and {n} (name) placeholders, in that order. Every segment
// is escaped, "/" included: the vhost "/" is "%2F".
type Path struct {
	Kind string
	Segs []string
}

var errPath = errors.New("mgmt: endpoint not allowed")

// build returns the escaped path of p, or an error before any I/O when the
// kind is not allowlisted or the segments do not fit its template.
func (p Path) build() (string, error) {
	tmpl, ok := Allowed[p.Kind]
	if !ok {
		return "", errPath
	}
	return fill(tmpl, p.Segs)
}

// fill replaces the placeholders of tmpl with the escaped segs.
func fill(tmpl string, segs []string) (string, error) {
	parts := strings.Split(tmpl, "/")
	i := 0
	for j, part := range parts {
		if part != "{v}" && part != "{n}" {
			continue
		}
		if i >= len(segs) || segs[i] == "" {
			return "", errPath
		}
		parts[j] = url.PathEscape(segs[i])
		i++
	}
	if i != len(segs) {
		return "", errPath
	}
	return strings.Join(parts, "/"), nil
}
