package state

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Output runs a program and returns its standard output. sysexec.Runner provides it.
type Output interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// SystemdTimers lists Carryon's timers with `systemctl list-timers --output=json` (systemd 255 or
// newer). Timers that aren't scheduled are left out.
func SystemdTimers(r Output) Timers {
	return func(ctx context.Context) ([]NextRun, error) {
		out, err := r.Output(ctx, "systemctl", "list-timers", "--all", "--no-pager", "--output=json", "carryon-*")
		if err != nil {
			return nil, err
		}
		return ParseTimers(out)
	}
}

// ParseTimers reads the output of `systemctl list-timers --output=json`.
func ParseTimers(data []byte) ([]NextRun, error) {
	var list []struct {
		Next *int64 `json:"next"` // microseconds since the epoch; null when not scheduled
		Unit string `json:"unit"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading systemd's timer list: %w", err)
	}
	var out []NextRun
	for _, t := range list {
		if t.Next == nil || *t.Next <= 0 || t.Unit == "" {
			continue
		}
		out = append(out, NextRun{Unit: t.Unit, At: time.UnixMicro(*t.Next).UTC()})
	}
	return out, nil
}
