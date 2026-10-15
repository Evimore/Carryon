package record

import "sort"

// Diff compares two item lists by key. Each result is sorted by key.
func Diff(old, new []Item) (added, removed, changed []Item) {
	o := byKey(old)
	n := byKey(new)
	for _, k := range sortedKeys(n) {
		if prev, ok := o[k]; !ok {
			added = append(added, n[k])
		} else if !sameContent(prev, n[k]) {
			changed = append(changed, n[k])
		}
	}
	for _, k := range sortedKeys(o) {
		if _, ok := n[k]; !ok {
			removed = append(removed, o[k])
		}
	}
	return added, removed, changed
}

// ChangeLines describes a diff, one line per item: "+ " added, "- " removed, "~ " changed.
func ChangeLines(added, removed, changed []Item) []string {
	var lines []string
	for _, it := range added {
		lines = append(lines, "+ "+it.Describe())
	}
	for _, it := range removed {
		lines = append(lines, "- "+it.Describe())
	}
	for _, it := range changed {
		lines = append(lines, "~ "+it.Describe())
	}
	return lines
}

func byKey(items []Item) map[string]Item {
	m := make(map[string]Item, len(items))
	for _, it := range items {
		m[it.Key()] = it
	}
	return m
}

func sortedKeys(m map[string]Item) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
