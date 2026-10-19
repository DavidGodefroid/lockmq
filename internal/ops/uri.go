package ops

import "regexp"

// uriUserinfo matches the userinfo of a URI: a scheme, "://", then
// everything up to the first "@" before the authority ends.
var uriUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/?#@\s]*@`)

// StripUserinfo removes the user and password of every URI in s
// ("amqps://u:p@h:5671/v" → "amqps://h:5671/v").
func StripUserinfo(s string) string {
	return uriUserinfo.ReplaceAllString(s, "$1")
}

// stripAll applies StripUserinfo to every string of a decoded JSON value,
// keys included.
func stripAll(v any) any {
	switch x := v.(type) {
	case string:
		return StripUserinfo(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = stripAll(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[StripUserinfo(k)] = stripAll(e)
		}
		return out
	default:
		return v
	}
}
