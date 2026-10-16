package restore

import (
	"bytes"
	"regexp"
	"sort"
)

// Adapter rewrites the recorded users' home paths, and User= lines naming them, for the users who
// receive their files here. Ported from adapt.
type Adapter struct {
	pairs []UserMap // users whose name or home differs, longest recorded home first
}

// NewAdapter returns an adapter for the user mapping. Users who keep their name and home need
// nothing rewritten.
func NewAdapter(users []UserMap) *Adapter {
	a := &Adapter{}
	for _, m := range users {
		if m.From.Home != "" && m.Changed() {
			a.pairs = append(a.pairs, m)
		}
	}
	sort.SliceStable(a.pairs, func(i, j int) bool { return len(a.pairs[i].From.Home) > len(a.pairs[j].From.Home) })
	return a
}

// Active reports whether anything gets rewritten.
func (a *Adapter) Active() bool { return a != nil && len(a.pairs) > 0 }

// userLine matches "User=alice", "AutoUser = alice", "username=alice"... as the prototype did
// (case-insensitive key; the value must be the whole rest of the line).
var userLine = regexp.MustCompile(`(?im)^([ \t]*(?:auto)?user(?:name)?[ \t]*=[ \t]*)([^ \t\r\n]+)([ \t]*\r?)$`)

// Bytes returns data with the recorded homes pointed at the new ones. A home matches only where
// it isn't followed by more of a name ("/home/alice" doesn't match in "/home/alice2"). Binary data
// (anything with a NUL byte) is returned unchanged.
func (a *Adapter) Bytes(data []byte) []byte {
	if !a.Active() || bytes.IndexByte(data, 0) >= 0 {
		return data
	}
	var out bytes.Buffer
	changed := false
	for i := 0; i < len(data); {
		hit := false
		for _, p := range a.pairs {
			h := p.From.Home
			if bytes.HasPrefix(data[i:], []byte(h)) && !nameByte(data, i+len(h)) {
				out.WriteString(p.To.Home)
				i += len(h)
				hit, changed = true, true
				break
			}
		}
		if !hit {
			out.WriteByte(data[i])
			i++
		}
	}
	res := data
	if changed {
		res = out.Bytes()
	}
	renamed := false
	for _, p := range a.pairs {
		if p.From.Name != p.To.Name {
			renamed = true
		}
	}
	if !renamed {
		return res
	}
	return userLine.ReplaceAllFunc(res, func(line []byte) []byte {
		m := userLine.FindSubmatch(line)
		for _, p := range a.pairs {
			if p.From.Name != p.To.Name && bytes.EqualFold(m[2], []byte(p.From.Name)) {
				return append(append(append([]byte{}, m[1]...), p.To.Name...), m[3]...)
			}
		}
		return line
	})
}

// String adapts text, such as a symlink's target.
func (a *Adapter) String(s string) string { return string(a.Bytes([]byte(s))) }

// nameByte reports whether data[i] continues a name: a letter, digit, '_', '.' or '-'.
func nameByte(data []byte, i int) bool {
	if i >= len(data) {
		return false
	}
	c := data[i]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}
