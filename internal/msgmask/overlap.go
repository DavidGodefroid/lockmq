package msgmask

import "strings"

// BuildMask validates a mask rule as a file holds it.
func BuildMask(r RuleSpec) (Rule, error) { return build(r, true) }

// Overlaps reports whether the allow rule a may name a value the mask rule
// m masks: the same kind of target (body path, header glob or property),
// paths or globs that may meet, and queues that may meet. A mask rule on an
// exchange masks a message read from any queue. It errs towards true: it
// refuses an allow rule that could weaken a mask the user cannot remove.
func Overlaps(a, m Rule) bool {
	switch {
	case a.Body != "" && m.Body != "":
		if !pathsMeet(a.path, m.path) {
			return false
		}
	case a.Header != "" && m.Header != "":
		if !globsMeet(a.Header, m.Header) {
			return false
		}
	case a.Property != "" && m.Property != "":
		if a.Property != m.Property {
			return false
		}
	default:
		return false
	}
	if len(m.Exchanges) > 0 {
		return true
	}
	for _, qa := range a.Queues {
		for _, qm := range m.Queues {
			if namesMeet(qa, qm) {
				return true
			}
		}
	}
	return false
}

// pathsMeet reports whether two rule paths name a common value: one covers
// a path below the other.
func pathsMeet(a, b jpath) bool {
	for i := 0; i < min(len(a), len(b)); i++ {
		x, y := a[i], b[i]
		switch {
		case x.kind == stepKey || y.kind == stepKey:
			if x.kind != y.kind || x.key != y.key {
				return false
			}
		case x.kind == stepIndex && y.kind == stepIndex:
			if x.index != y.index {
				return false
			}
		}
	}
	return true
}

// namesMeet reports whether two vhost/name globs may name a common queue.
func namesMeet(a, b string) bool {
	av, an := splitName(a)
	bv, bn := splitName(b)
	return globsMeet(av, bv) && globsMeet(an, bn)
}

func splitName(g string) (vhost, name string) {
	if rest, ok := strings.CutPrefix(g, "//"); ok {
		return "/", rest
	}
	vhost, name, _ = strings.Cut(g, "/")
	return vhost, name
}

// globsMeet reports whether two globs may match a common string: their
// literal prefixes, and their literal suffixes, agree.
func globsMeet(a, b string) bool {
	const wild = "*?"
	ia, ib := strings.IndexAny(a, wild), strings.IndexAny(b, wild)
	if ia < 0 && ib < 0 {
		return a == b
	}
	if ia < 0 {
		return glob(b, a)
	}
	if ib < 0 {
		return glob(a, b)
	}
	pa, pb := a[:ia], b[:ib]
	if !strings.HasPrefix(pa, pb) && !strings.HasPrefix(pb, pa) {
		return false
	}
	sa, sb := a[strings.LastIndexAny(a, wild)+1:], b[strings.LastIndexAny(b, wild)+1:]
	return strings.HasSuffix(sa, sb) || strings.HasSuffix(sb, sa)
}
