package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/notify"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

func newCaptureCmd(dirs paths.Dirs) *cobra.Command {
	var out string
	var quick bool
	cmd := &cobra.Command{
		Use:   "capture [--out DIR]",
		Short: "Refresh the system record without backing up",
		Long: `Look at this machine and refresh its system record (packages, changed system files,
services, users, language tools and backed-up paths) in ` + dirs.RecordDir() + `,
keeping its history. The next backup takes it along.

With --out DIR, the record goes into DIR instead, which must be empty, so you can look at it or
compare it with an older one.

Run it with sudo: without root, only --out works, files only root can read are skipped and the
record is marked incomplete.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" && geteuid() != 0 {
				return fmt.Errorf("refreshing the system record in %s needs root: run it with sudo, or give --out DIR to write one somewhere else", dirs.RecordDir())
			}
			if out != "" {
				if err := checkEmptyDir(out); err != nil {
					return err
				}
			}
			return runCapture(cmd, dirs, out, !quick)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the record into this empty folder instead of refreshing the machine's record")
	cmd.Flags().BoolVar(&quick, "quick", false, "leave out the slow checks, like the pacman hook does (edited package files, the not-backed-up list)")
	return cmd
}

// checkEmptyDir refuses a folder that has something in it.
func checkEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("can't use %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s isn't empty: give capture an empty or new folder, so nothing in it is overwritten", dir)
	}
	return nil
}

func runCapture(cmd *cobra.Command, dirs paths.Dirs, out string, nightly bool) error {
	log := ui.Stderr()
	cfg, err := captureConfig(dirs, log)
	if err != nil {
		return err
	}
	sys, err := backupSystem(cfg)
	if err != nil {
		return err
	}
	start := time.Now()
	res, err := backup.Capture(cmd.Context(), backup.CaptureOptions{
		Dirs: dirs, Config: cfg, System: sys, Nightly: nightly, Out: out, Log: log,
	})
	if out == "" {
		recordCapture(cmd, dirs, cfg, sys, start, res, err, log)
	}
	if err != nil {
		return err
	}
	printCaptureSummary(cmd.OutOrStdout(), res.Result)
	if res.Published {
		printChanges(cmd.OutOrStdout(), res.Publish)
	}
	return nil
}

// recordCapture adds a capture of the machine's record to the run history.
func recordCapture(cmd *cobra.Command, dirs paths.Dirs, cfg config.Config, sys backup.System, start time.Time, res *backup.Captured, err error, log *ui.Logger) {
	run := state.Run{Kind: state.KindCapture, Trigger: state.TriggerManual, Machine: machineName(cfg), Start: start, End: time.Now()}
	switch {
	case err != nil:
		run.Result, run.Error = state.ResultFailed, notify.Redact(err.Error())
	case len(res.Warnings) > 0:
		run.Result, run.Warnings = state.ResultWarning, res.Warnings
	default:
		run.Result = state.ResultOK
	}
	if res != nil {
		run.Changes = len(res.Publish.Changes())
	}
	host := backupHost(cmd.Context(), sys.Runner)
	backup.Record(cmd.Context(), dirs, run, run.Machine, host.Timers, log)
}

// captureConfig reads the settings, or uses the defaults before `carryon init` has written any.
func captureConfig(dirs paths.Dirs, log *ui.Logger) (config.Config, error) {
	file := dirs.ConfigFile()
	cfg, warnings, err := config.Load(file)
	if errors.Is(err, fs.ErrNotExist) {
		log.Info("%s doesn't exist yet: using the default settings", file)
		return config.Default(), nil
	}
	if err != nil {
		return config.Config{}, err
	}
	for _, w := range warnings {
		log.Warn("%s", w)
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("%s has problems:\n%w", file, err)
	}
	return cfg, nil
}

// maxChangeLines is how many change lines capture prints.
const maxChangeLines = 20

// printChanges says what changed in the published record.
func printChanges(w io.Writer, pub record.PublishResult) {
	lines := pub.Changes()
	switch {
	case pub.First:
		fmt.Fprintln(w, "This is the machine's first record.")
	case len(lines) == 0:
		fmt.Fprintln(w, "Nothing changed since the last capture.")
	default:
		fmt.Fprintf(w, "%s since the last capture:\n", plural(len(lines), "change"))
		for i, l := range lines {
			if i == maxChangeLines && len(lines) > maxChangeLines+1 {
				fmt.Fprintf(w, "  and %d more (carryon history shows them all)\n", len(lines)-i)
				break
			}
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
}

// printCaptureSummary says what the record holds: items per type and anything skipped.
func printCaptureSummary(w io.Writer, res *capture.Result) {
	counts := map[string]int{}
	for _, it := range res.Items {
		counts[it.Type]++
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Strings(types)
	var names []string
	for _, u := range res.Users {
		names = append(names, u.Name)
	}
	who := "no users"
	if len(names) > 0 {
		who = "users: " + strings.Join(names, ", ")
	}
	fmt.Fprintf(w, "System record written to %s (%d items, %s)\n", res.Dir, len(res.Items), who)
	for _, t := range types {
		fmt.Fprintf(w, "  %-8s %d\n", t, counts[t])
	}
	if len(res.Skipped) > 0 {
		verb := "were"
		if len(res.Skipped) == 1 {
			verb = "was"
		}
		fmt.Fprintf(w, "%s couldn't be read and %s skipped (listed in %s), so the record is incomplete.\n",
			plural(len(res.Skipped), "path"), verb, filepath.Join(res.Dir, capture.SkippedFile))
	} else if !res.Meta.Complete {
		fmt.Fprintln(w, "The record is incomplete because it was captured without root: run it with sudo.")
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintf(w, "%s (shown above).\n", plural(len(res.Warnings), "warning"))
	}
}

// plural is "1 path" or "2 paths".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
