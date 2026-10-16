package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/state"
)

func TestSettle(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	// No marker: nothing to do.
	if out, _, err := Settle(ctx, env.options()); out != SettleNothing || err != nil {
		t.Fatalf("no marker: %v %v", out, err)
	}

	// A change 10 minutes ago waits for the delay (1h by default).
	if err := state.MarkPending(env.dirs, state.ReasonChange, env.now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if out, _, err := Settle(ctx, env.options()); out != SettleTooSoon || err != nil {
		t.Fatalf("recent change: %v %v", out, err)
	}
	if env.checker.calls != 0 {
		t.Error("settle checked the destination too soon")
	}

	// A destination that's away keeps the marker, as waiting, without an error the unit reports.
	env.now = env.now.Add(2 * time.Hour)
	env.checker.res = dest.Result{Status: dest.NotMounted, Message: "No drive is mounted."}
	out, res, err := Settle(ctx, env.options())
	if out != SettleRan || !errors.Is(err, ErrWaiting) || res.Run.Trigger != state.TriggerSettle {
		t.Fatalf("waiting: %v %v %+v", out, err, res.Run)
	}
	if p, ok, _ := state.ReadPending(env.dirs); !ok || p.Reason != state.ReasonWaiting {
		t.Fatalf("pending = %+v %v", p, ok)
	}

	// A waiting backup runs as soon as the destination is back, and clears the marker.
	env.now = env.now.Add(time.Minute)
	env.checker.res = dest.Result{Status: dest.OK}
	out, res, err = Settle(ctx, env.options())
	if out != SettleRan || err != nil || !res.Run.Succeeded() {
		t.Fatalf("back: %v %v %q", out, err, res.Run.Result)
	}
	if _, ok, _ := state.ReadPending(env.dirs); ok {
		t.Error("the marker is still there")
	}

	// A failed backup keeps the marker and pushes the next try back by the delay.
	if err := state.MarkPending(env.dirs, state.ReasonChange, env.now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	env.checker.res = dest.Result{Status: dest.RepoMismatch, Message: "Not the repository Carryon set up."}
	out, _, err = Settle(ctx, env.options())
	if out != SettleRan || err == nil {
		t.Fatalf("failure: %v %v", out, err)
	}
	if p, ok, _ := state.ReadPending(env.dirs); !ok || !p.Since.Equal(env.now) {
		t.Errorf("pending after a failure = %+v %v, want since %v", p, ok, env.now)
	}

	// While restoring, settle does nothing.
	write(t, env.dirs.RestoringFlag(), "")
	env.checker.calls = 0
	env.now = env.now.Add(3 * time.Hour)
	if out, _, err := Settle(ctx, env.options()); out != SettleNothing || err != nil || env.checker.calls != 0 {
		t.Fatalf("restoring: %v %v", out, err)
	}
}

func TestAfterChangeDelay(t *testing.T) {
	cfg := config.Default()
	for _, tc := range []struct {
		in   string
		d    time.Duration
		off  bool
		fail bool
	}{
		{"1h", time.Hour, false, false},
		{"30min", 30 * time.Minute, false, false},
		{"off", 0, true, false},
		{"0", 0, false, false},
		{"soon", 0, false, true},
	} {
		cfg.Backup.AfterChangeDelay = tc.in
		d, off, err := afterChangeDelay(cfg)
		if d != tc.d || off != tc.off || (err != nil) != tc.fail {
			t.Errorf("%q: %v %v %v", tc.in, d, off, err)
		}
	}
}
