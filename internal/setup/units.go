package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/sysexec"
)

// Timers are the units init enables (they come with the package, from dist/): the timers, and
// the watcher that refreshes the record when Flatpak apps change.
var Timers = []string{"carryon-backup.timer", "carryon-verify.timer", "carryon-settle.timer", "carryon-flatpak.path"}

// AgentUnit is the user unit init switches on for every user: carryon agent, the desktop
// notifications with buttons. It starts with each user's desktop session.
const AgentUnit = "carryon-agent.service"

// scheduleDropIn is the drop-in that sets a timer's OnCalendar= from a config setting.
func scheduleDropIn(setting, schedule, configFile string) []byte {
	return fmt.Appendf(nil, `# Written by carryon init from %s in %s.
# Change the setting there and run "sudo carryon init" again, rather than editing this file.
[Timer]
OnCalendar=
OnCalendar=%s
`, setting, configFile, schedule)
}

// checkSchedule refuses a schedule that can't be one line of a unit file.
func checkSchedule(s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return fmt.Errorf("the schedule is empty")
	case strings.ContainsAny(s, "\n\r"):
		return fmt.Errorf("the schedule must fit on one line")
	}
	return nil
}

// installUnits writes the schedules into drop-ins and enables the timers. Problems with systemd are
// warnings: the settings are already saved, and the message says how to finish by hand.
func (e *Env) installUnits(ctx context.Context, cfg config.Config) {
	for _, d := range []struct{ unit, setting, schedule string }{
		{"carryon-backup.timer", "backup.schedule", cfg.Backup.Schedule},
		{"carryon-verify.timer", "verify.schedule", cfg.Verify.Schedule},
	} {
		path := filepath.Join(e.systemdDir(), d.unit+".d", "schedule.conf")
		if err := writePublic(path, scheduleDropIn(d.setting, d.schedule, e.Dirs.ConfigFile())); err != nil {
			e.Log.Warn("Couldn't save the schedule for %s: %v", d.unit, err)
		}
	}

	run := e.runner()
	enable := "sudo systemctl enable --now " + strings.Join(Timers, " ")
	if !run.Exists("systemctl") {
		e.Log.Warn("systemctl isn't available, so automatic backups aren't switched on. On a system with systemd, run: %s", enable)
		return
	}
	if _, err := run.Output(ctx, "systemctl", "daemon-reload"); err != nil {
		e.Log.Warn("systemd couldn't reload its units, so automatic backups aren't switched on: %s\nOnce systemd runs, run: sudo systemctl daemon-reload && %s", lastLine(err), enable)
		return
	}
	var missing []string
	for _, t := range Timers {
		if _, err := run.Output(ctx, "systemctl", "enable", "--now", t); err != nil {
			msg := lastLine(err)
			if strings.Contains(msg, "does not exist") || strings.Contains(msg, "not found") {
				missing = append(missing, t)
				continue
			}
			e.Log.Warn("Couldn't switch on %s: %s\nFix that, then run: sudo systemctl enable --now %s", t, msg, t)
			continue
		}
		e.Log.Info("Switched on %s.", t)
	}
	if len(missing) > 0 {
		e.Log.Warn("%s %s not installed, so %s. Install Carryon from its package (it brings the units), then run: sudo systemctl enable --now %s",
			strings.Join(missing, ", "), plural(len(missing), "is", "are"), whatStops(missing), strings.Join(missing, " "))
	}
	e.enableAgent(ctx, run)
}

// enableAgent switches the user agent on for every user (systemctl --global), so it starts with
// each desktop session from the next login on. Without it, failures still arrive as plain
// notifications from root.
func (e *Env) enableAgent(ctx context.Context, run sysexec.Runner) {
	enable := "sudo systemctl --global enable " + AgentUnit
	_, err := run.Output(ctx, "systemctl", "--global", "enable", AgentUnit)
	switch {
	case err == nil:
		e.Log.Info("Switched on %s for every user: desktop notifications with buttons start at their next login.", AgentUnit)
	case strings.Contains(lastLine(err), "does not exist") || strings.Contains(lastLine(err), "not found"):
		e.Log.Warn("%s isn't installed, so desktop notifications come without buttons. Install Carryon from its package (it brings the unit), then run: %s", AgentUnit, enable)
	default:
		e.Log.Warn("Couldn't switch on %s: %s\nFix that, then run: %s", AgentUnit, lastLine(err), enable)
	}
}

// whatStops says what doesn't happen without the missing timers.
func whatStops(missing []string) string {
	var parts []string
	for _, t := range missing {
		switch t {
		case "carryon-backup.timer":
			parts = append(parts, "daily backups won't run")
		case "carryon-verify.timer":
			parts = append(parts, "the weekly check won't run")
		case "carryon-settle.timer":
			parts = append(parts, "backups after package changes won't run")
		case "carryon-flatpak.path":
			parts = append(parts, "Flatpak changes won't refresh the system record")
		}
	}
	return strings.Join(parts, " and ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// lastLine is the last non-empty line of a program's error, which usually says what went wrong.
func lastLine(err error) string {
	text := err.Error()
	var ee *sysexec.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(ee.Stderr) != "" {
		text = ee.Stderr
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
