package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var spanUnits = map[string]time.Duration{
	"s": time.Second, "sec": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
}

// ParseSpan parses a systemd-style time span such as "1h", "30min", "2d" or "1h 30min". A number
// without a unit is seconds, as in systemd.
func ParseSpan(s string) (time.Duration, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, fmt.Errorf("empty time span")
	}
	var total time.Duration
	rs := []rune(in)
	for i := 0; i < len(rs); {
		for i < len(rs) && unicode.IsSpace(rs[i]) {
			i++
		}
		start := i
		for i < len(rs) && unicode.IsDigit(rs[i]) {
			i++
		}
		if start == i {
			return 0, fmt.Errorf("time span %q: expected a number at %q", s, string(rs[start:]))
		}
		n, err := strconv.ParseInt(string(rs[start:i]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("time span %q: %v", s, err)
		}
		for i < len(rs) && unicode.IsSpace(rs[i]) {
			i++
		}
		ustart := i
		for i < len(rs) && unicode.IsLetter(rs[i]) {
			i++
		}
		unit := time.Second
		if u := string(rs[ustart:i]); u != "" {
			var ok bool
			if unit, ok = spanUnits[u]; !ok {
				return 0, fmt.Errorf("time span %q: unknown unit %q (use s, min, h, d or w)", s, u)
			}
		}
		total += time.Duration(n) * unit
	}
	return total, nil
}
