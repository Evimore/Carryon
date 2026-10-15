package config

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// A migration upgrades a config file from one config_version to the next. It works on the file's
// raw settings, so it can rename or reshape settings the current Config no longer has.
type migration struct {
	note  string                     // what changed, in words the user recognizes
	apply func(map[string]any) error // edits the raw settings in place
}

// migrations[v] upgrades version v to v+1. Version 1 is the first, so there are none yet. When
// the format changes: add a step here, bump CurrentVersion, and keep old steps forever.
var migrations = map[int]migration{}

// migrate upgrades a config file's text to the target version. Files already at the target, and
// files without config_version, come back unchanged, so errors keep their line numbers. A file
// that doesn't parse also comes back unchanged, for the caller's decoder to report.
func migrate(text string, steps map[int]migration, target int) (string, []string, error) {
	var raw map[string]any
	if _, err := toml.Decode(text, &raw); err != nil {
		return text, nil, nil
	}
	v, ok := raw["config_version"].(int64)
	if !ok {
		return text, nil, nil
	}
	if v < 1 {
		return "", nil, fmt.Errorf("config_version %d isn't valid: the first version is 1", v)
	}
	if v >= int64(target) {
		return text, nil, nil
	}
	var notes []string
	for ; v < int64(target); v++ {
		step, ok := steps[int(v)]
		if !ok {
			return "", nil, fmt.Errorf("config_version %d can't be upgraded to %d by this Carryon: write the file again with \"carryon init\"", v, target)
		}
		if err := step.apply(raw); err != nil {
			return "", nil, fmt.Errorf("upgrading config_version %d to %d: %w", v, v+1, err)
		}
		raw["config_version"] = v + 1
		notes = append(notes, fmt.Sprintf("upgraded from config_version %d to %d: %s", v, v+1, step.note))
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return "", nil, fmt.Errorf("upgrading config_version: %w", err)
	}
	return buf.String(), notes, nil
}

// moveSetting renames a setting in raw settings, such as "backup.delay" to
// "backup.after_change_delay". It does nothing when the old setting isn't there, and keeps a new
// setting the file already has.
func moveSetting(raw map[string]any, from, to string) {
	fromTable, fromKey := table(raw, from, false)
	if fromTable == nil {
		return
	}
	val, ok := fromTable[fromKey]
	if !ok {
		return
	}
	delete(fromTable, fromKey)
	toTable, toKey := table(raw, to, true)
	if _, exists := toTable[toKey]; !exists {
		toTable[toKey] = val
	}
}

// table finds the table holding a dotted key and the key's last part, creating missing tables
// when create is set.
func table(raw map[string]any, key string, create bool) (map[string]any, string) {
	parts := strings.Split(key, ".")
	t := raw
	for _, p := range parts[:len(parts)-1] {
		next, ok := t[p].(map[string]any)
		if !ok {
			if !create {
				return nil, ""
			}
			next = map[string]any{}
			t[p] = next
		}
		t = next
	}
	return t, parts[len(parts)-1]
}
