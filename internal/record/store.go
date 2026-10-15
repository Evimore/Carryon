package record

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Files in a record directory.
const (
	MetaFile    = "meta.json"
	ItemsFile   = "items.jsonl"
	BundlesFile = "bundles.json"
)

// Record is a machine's record as stored on disk.
type Record struct {
	Meta    Meta
	Items   []Item
	Bundles Bundles
}

// Load reads the record in dir. It reads records written by the prototype (format 1) too.
func Load(dir string) (*Record, error) {
	var r Record
	if err := readJSON(filepath.Join(dir, MetaFile), &r.Meta); err != nil {
		return nil, err
	}
	if r.Meta.Format < FormatPrototype || r.Meta.Format > FormatCurrent {
		return nil, fmt.Errorf("%s: record format %d isn't supported (this Carryon reads formats %d to %d)",
			dir, r.Meta.Format, FormatPrototype, FormatCurrent)
	}
	f, err := os.Open(filepath.Join(dir, ItemsFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if r.Items, err = ReadItems(f); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, ItemsFile), err)
	}
	if err := readJSON(filepath.Join(dir, BundlesFile), &r.Bundles); err != nil {
		return nil, err
	}
	return &r, nil
}

// Save writes the record's three files into dir, which must exist. It doesn't write them
// atomically; publishing a new record over an old one is the record store's job.
func (r *Record) Save(dir string) error {
	var items bytes.Buffer
	if err := WriteItems(&items, r.Items); err != nil {
		return err
	}
	meta, err := json.MarshalIndent(r.Meta, "", "  ")
	if err != nil {
		return err
	}
	bundles, err := marshalBundles(r.Bundles)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		MetaFile:    append(meta, '\n'),
		ItemsFile:   items.Bytes(),
		BundlesFile: bundles,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ReadItems reads items, one JSON object per line. Blank lines are skipped.
func ReadItems(r io.Reader) ([]Item, error) {
	var items []Item
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // a repository section or long path can make a long line
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var it Item
		if err := json.Unmarshal(line, &it); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if it.Type == "" || it.ID == "" {
			return nil, fmt.Errorf("line %d: an item needs a type and an id", n)
		}
		items = append(items, it)
	}
	return items, sc.Err()
}

// WriteItems writes items, one JSON object per line with sorted keys.
func WriteItems(w io.Writer, items []Item) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	return nil
}

func marshalBundles(bs Bundles) ([]byte, error) {
	raw, err := json.Marshal(bs)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
