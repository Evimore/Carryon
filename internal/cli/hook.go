package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/detect"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

// hookTimeout bounds the pacman hook; tests shorten it.
var hookTimeout = detect.HookTimeout

// hookInboxWait is how long the hook waits for the inbox lock. A busy inbox skips the suggestion,
// so pacman isn't held up; the nightly backup finds it again.
const hookInboxWait = 2 * time.Second

// hookStdinTimeout is how long the hook waits for pacman's list of packages.
const hookStdinTimeout = 5 * time.Second

func newHookCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "hook",
		Short: "Called by the pacman hook",
		Long: `Runs after every package install, upgrade or removal (carryon.hook): refreshes the system
record, notes that a backup is owed, looks for new data in the folders of the packages it was
given and prints hints. It always exits successfully and gives up
after ` + hookTimeout.String() + `, so it never breaks or holds up pacman.`,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runHook(cmd.Context(), dirs, cmd.InOrStdin(), cmd.OutOrStdout())
			return nil
		},
	}
}

// runHook is the pacman hook. It never fails and never panics: what goes wrong becomes a hint.
func runHook(ctx context.Context, dirs paths.Dirs, stdin io.Reader, out io.Writer) {
	defer func() {
		if p := recover(); p != nil {
			hint(out, "Carryon ran into a bug in its pacman hook (%v). Your packages are fine.", p)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()

	cfg, warnings, err := config.Load(dirs.ConfigFile())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return // Carryon isn't set up here: nothing to do
	case err != nil:
		hint(out, "Carryon's settings can't be read, so the system record wasn't refreshed: %v", err)
		return
	}
	_ = warnings // carryon config check shows them; pacman's output isn't the place
	if state.Restoring(dirs) {
		return // a restore is in progress: automatic runs wait for it
	}
	if geteuid() != 0 {
		hint(out, "carryon hook refreshes the system record as root; pacman runs it by itself.")
		return
	}
	packages := readTargets(stdin)
	sys, err := backupSystem(cfg)
	if err != nil {
		hint(out, "Carryon couldn't refresh the system record: %v", err)
		return
	}
	machine := machineName(cfg)
	timers := hookTimers(sys)
	quiet := ui.NewLogger(io.Discard, false, false)
	var captured *backup.Captured // the refresh's, for the scan

	opt := detect.HookOptions{
		Packages: packages,
		Delay:    cfg.Backup.AfterChangeDelay,
		Out:      out,
		Timeout:  hookTimeout,
		Refresh: func(ctx context.Context) ([]string, error) {
			res, err := backup.Capture(ctx, backup.CaptureOptions{Dirs: dirs, Config: cfg, System: sys, Log: quiet})
			if err != nil {
				return nil, err
			}
			captured = res
			return res.Publish.Changes(), nil
		},
		Scan: func(ctx context.Context, packages []string) (detect.Outcome, error) {
			if captured == nil || captured.Result == nil {
				return detect.Outcome{}, nil // no fresh record: the nightly backup looks instead
			}
			return backup.Detect(ctx, backup.DetectOptions{
				Dirs: dirs, Config: cfg, System: sys,
				Items: captured.Result.Items, Users: captured.Result.Users,
				Excludes: captured.Excludes, Mounts: captured.Mounts,
				Only: packages, By: state.ByHook, Wait: hookInboxWait,
			})
		},
		Offer: hookOffer(dirs, sys, packages),
		Record: func(r state.Run) {
			r.Machine, r.Unit = machine, ""
			backup.Record(ctx, dirs, r, machine, timers, quiet)
		},
	}
	// A backup follows the change once it settles, when backups are set up and that's on.
	if _, err := state.ReadDestination(dirs); err == nil && !backup.AfterChangeOff(cfg) {
		opt.Pending = func() error { return state.MarkPending(dirs, state.ReasonChange, time.Now()) }
	}
	detect.Hook(ctx, opt)
}

// readTargets reads the package names pacman passes on standard input (NeedsTargets), giving up
// after a few seconds so a hook run by hand never waits for a terminal.
func readTargets(stdin io.Reader) []string {
	if stdin == nil {
		return nil
	}
	if f, ok := stdin.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice != 0 {
			return nil
		}
	}
	got := make(chan []string, 1)
	go func() {
		var names []string
		sc := bufio.NewScanner(io.LimitReader(stdin, 1<<20))
		for sc.Scan() {
			if name := strings.TrimSpace(sc.Text()); name != "" {
				names = append(names, name)
			}
		}
		got <- names
	}()
	select {
	case names := <-got:
		return names
	case <-time.After(hookStdinTimeout):
		return nil
	}
}

// hint prints one line for pacman to show, ignoring write errors.
func hint(out io.Writer, format string, a ...any) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, format+"\n", a...)
}

// hookTimers tells status.json when the timers fire next; tests replace it.
var hookTimers = func(sys backup.System) state.Timers { return state.SystemdTimers(sys.Runner) }
