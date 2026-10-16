package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

// RunHistory reads a unit's error from Carryon's own run history, which outlives the journal on
// distributions that keep it short. When the history has no failed run for the unit's latest
// invocation (the run crashed before recording itself, or isn't one of Carryon's), it asks
// Fallback, normally the Journal.
type RunHistory struct {
	Dirs paths.Dirs
	// Runner looks up the unit's latest invocation, so an older run's error isn't reported for a
	// newer failure. Nil skips that check.
	Runner sysexec.Runner
	// Fallback is asked when the history has nothing. Nil means "".
	Fallback ErrorSource
}

// LastError returns the error and fix of the unit's latest run when it failed, else Fallback's
// answer.
func (h RunHistory) LastError(ctx context.Context, unit string) (string, error) {
	if text := h.fromHistory(ctx, unit); text != "" {
		return text, nil
	}
	if h.Fallback == nil {
		return "", nil
	}
	return h.Fallback.LastError(ctx, unit)
}

func (h RunHistory) fromHistory(ctx context.Context, unit string) string {
	runs, err := state.ReadRuns(h.Dirs)
	if err != nil || len(runs) == 0 {
		return ""
	}
	inv := ""
	if h.Runner != nil {
		if out, err := h.Runner.Output(ctx, "systemctl", "show", "-p", "InvocationID", "--value", unit); err == nil {
			inv = strings.TrimSpace(string(out))
		}
	}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.Unit != unit {
			continue
		}
		if inv != "" && r.Invocation != "" && r.Invocation != inv {
			return "" // the failed invocation recorded nothing
		}
		if r.Succeeded() || r.Error == "" {
			return ""
		}
		text := r.Error
		if r.Fix != "" {
			text = fmt.Sprintf("%s\n%s", r.Error, r.Fix)
		}
		return lastLines(text, errorLines)
	}
	return ""
}
