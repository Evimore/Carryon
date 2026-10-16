package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/state"
)

// SettleOutcome says what Settle did.
type SettleOutcome int

const (
	// SettleNothing: no backup is owed, or automatic runs are paused for a restore.
	SettleNothing SettleOutcome = iota
	// SettleTooSoon: the last change is newer than backup.after_change_delay.
	SettleTooSoon
	// SettleBusy: another backup is running; it covers the change.
	SettleBusy
	// SettleRan: the backup ran (see the result and error for how it went).
	SettleRan
)

// Settle backs up once the last change is older than backup.after_change_delay, or as soon as
// the destination is back for a backup that waited for it. carryon-settle.timer starts it every
// few minutes while the pending marker exists. The backup clears the marker; when it fails, the
// marker gets the current time, so the next try (and the next alert) waits another delay.
func Settle(ctx context.Context, opt Options) (SettleOutcome, Result, error) {
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	p, ok, err := state.ReadPending(opt.Dirs)
	if err != nil {
		return SettleNothing, Result{}, fmt.Errorf("reading the pending marker: %w", err)
	}
	if !ok || state.Restoring(opt.Dirs) {
		return SettleNothing, Result{}, nil
	}
	if p.Reason == state.ReasonChange {
		delay, off, err := afterChangeDelay(opt.Config)
		if err != nil {
			return SettleNothing, Result{}, err
		}
		if off || now().Sub(p.Since) < delay {
			return SettleTooSoon, Result{}, nil
		}
	}
	opt.Trigger = state.TriggerSettle
	res, err := Run(ctx, opt)
	switch {
	case errors.Is(err, state.ErrBusy):
		return SettleBusy, res, nil
	case errors.Is(err, ErrWaiting):
		return SettleRan, res, err
	case err != nil:
		if merr := state.MarkPending(opt.Dirs, state.ReasonChange, now()); merr != nil {
			err = errors.Join(err, merr)
		}
	}
	return SettleRan, res, err
}

// afterChangeDelay reads backup.after_change_delay. off is true when backups after changes are
// switched off.
func afterChangeDelay(cfg config.Config) (d time.Duration, off bool, err error) {
	v := strings.TrimSpace(cfg.Backup.AfterChangeDelay)
	switch strings.ToLower(v) {
	case "off", "no", "never":
		return 0, true, nil
	case "", "0":
		return 0, false, nil
	}
	d, err = config.ParseSpan(v)
	if err != nil {
		return 0, false, fmt.Errorf("backup.after_change_delay: %w", err)
	}
	return d, false, nil
}

// AfterChangeOff reports whether backups after package changes are switched off.
func AfterChangeOff(cfg config.Config) bool {
	_, off, _ := afterChangeDelay(cfg)
	return off
}
