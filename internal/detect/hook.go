// Package detect notices changes to the machine. For now it holds what the pacman hook does after
// every install, upgrade or removal; the scans for new data folders come with v0.2. See
// docs/design.md, "Running by itself" and "Noticing new things".
package detect

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
)

// HookTimeout is how long the pacman hook may take. pacman waits for its hooks, so the hook gives
// up rather than hold up the transaction.
const HookTimeout = 2 * time.Minute

// HookOptions configure the pacman hook.
type HookOptions struct {
	// Refresh captures the machine quickly and publishes the record, returning the change lines.
	Refresh func(ctx context.Context) (changes []string, err error)
	// Pending notes that a backup is owed. Nil means no backup follows changes
	// (backup.after_change_delay is off).
	Pending func() error
	// Record adds the hook's run to the history. Nil means it isn't recorded.
	Record func(state.Run)
	// Packages are the packages pacman changed, from its standard input.
	Packages []string
	// Delay is backup.after_change_delay, for the hint.
	Delay string
	// Out receives the hints pacman shows.
	Out io.Writer
	// Timeout bounds the whole hook. Zero means HookTimeout.
	Timeout time.Duration
	Now     func() time.Time
}

// Hook does the pacman hook's work: it notes that a backup is owed, refreshes the record and
// prints hints. It never fails, never panics and returns within the time limit, so it can't break
// or block a pacman transaction; what goes wrong becomes a hint.
func Hook(ctx context.Context, opt HookOptions) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = HookTimeout
	}
	out := &hintWriter{w: opt.Out}
	defer out.close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				out.hint("Carryon ran into a bug while refreshing the system record (%v). Your packages are fine; the next backup refreshes it.", p)
			}
		}()
		hook(ctx, opt, out)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		out.hint("Carryon stopped refreshing the system record after %s, so pacman isn't held up. Your packages are fine; the next backup refreshes it.", timeout)
	}
}

func hook(ctx context.Context, opt HookOptions, out *hintWriter) {
	run := state.Run{Kind: state.KindCapture, Trigger: state.TriggerHook, Start: opt.Now()}
	pendingErr := error(nil)
	if opt.Pending != nil {
		pendingErr = opt.Pending()
	}
	changes, err := safeRefresh(ctx, opt.Refresh)
	run.End = opt.Now()
	switch {
	case ctx.Err() != nil:
		return // Hook says so
	case err != nil:
		run.Result, run.Error = state.ResultFailed, err.Error()
		out.hint("Carryon couldn't refresh the system record: %v. Your packages are fine; the next backup refreshes it.", err)
	default:
		run.Result, run.Changes = state.ResultOK, len(changes)
		if len(changes) > 0 {
			out.hint("Carryon: the system record has %d change(s): %s", len(changes), record.Brief(changes, 3))
		}
	}
	switch {
	case pendingErr != nil:
		run.Warnings = append(run.Warnings, pendingErr.Error())
		out.hint("Carryon couldn't note that a backup is owed (%v); the next scheduled backup includes these changes.", pendingErr)
	case opt.Pending != nil:
		out.hint("Carryon backs up these changes %s.", whenHint(opt.Delay))
	}
	if run.Result == state.ResultOK && len(run.Warnings) > 0 {
		run.Result = state.ResultWarning
	}
	run.Packages = opt.Packages
	if opt.Record != nil && ctx.Err() == nil {
		opt.Record(run)
	}
}

// safeRefresh runs refresh, turning a panic into an error.
func safeRefresh(ctx context.Context, refresh func(context.Context) ([]string, error)) (changes []string, err error) {
	if refresh == nil {
		return nil, nil
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("a bug stopped it: %v", p)
		}
	}()
	return refresh(ctx)
}

// whenHint says when a backup follows a change.
func whenHint(delay string) string {
	delay = strings.TrimSpace(delay)
	if delay == "" || delay == "0" {
		return "in a few minutes"
	}
	return "once nothing has changed for " + delay
}

// hintWriter writes pacman's hints, one per line with the hook's arrow, and drops writes after the
// hook has returned.
type hintWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (h *hintWriter) hint(format string, a ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.w == nil {
		return
	}
	msg := strings.TrimSpace(fmt.Sprintf(format, a...))
	for _, line := range strings.Split(msg, "\n") {
		// Showing hints matters less than finishing: ignore write errors.
		_, _ = fmt.Fprintln(h.w, line)
	}
}

func (h *hintWriter) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
}
