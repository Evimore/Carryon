package record

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Bundle is a group of items restored together.
type Bundle struct {
	ID    string
	Title string // shown in the restore checklist
	When  string // when-rule deciding whether it starts ticked (see package rules)
}

// Bundles is an ordered list of bundles: the order is the restore order.
//
// In JSON it's an object keyed by bundle ID, {"id": {"title": ..., "when": ...}, ...}, whose key
// order is kept when reading and writing.
type Bundles []Bundle

type bundleJSON struct {
	Title string `json:"title"`
	When  string `json:"when"`
}

// Get returns the bundle with the given ID.
func (bs Bundles) Get(id string) (Bundle, bool) {
	for _, b := range bs {
		if b.ID == id {
			return b, true
		}
	}
	return Bundle{}, false
}

// MarshalJSON writes the bundles as an object, in order.
func (bs Bundles) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, b := range bs {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(b.ID)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(bundleJSON{Title: b.Title, When: b.When})
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads an object of bundles, keeping the order of its keys.
func (bs *Bundles) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("bundles: want an object, got %v", tok)
	}
	var out Bundles
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		id, ok := tok.(string)
		if !ok {
			return fmt.Errorf("bundles: want a bundle id, got %v", tok)
		}
		var v bundleJSON
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("bundle %q: %w", id, err)
		}
		if seen[id] {
			return fmt.Errorf("bundle %q appears twice", id)
		}
		seen[id] = true
		out = append(out, Bundle{ID: id, Title: v.Title, When: v.When})
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return err
	}
	*bs = out
	return nil
}
