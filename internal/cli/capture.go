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

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

func newCaptureCmd(dirs paths.Dirs) *cobra.Command {
	var out string
	var quick bool
	cmd := &cobra.Command{
		Use:   "capture --out DIR",
		Short: "Refresh the system record without backing up",
		Long: `Look at this machine and write its system record (packages, changed system files,
services, users, language tools and backed-up paths) into DIR, which must be empty.

For now the record only goes to --out, so you can look at it or compare it with the record the
sysbackup prototype wrote. Publishing it into ` + dirs.RecordDir() + ` comes with the backup run.

Run it with sudo: without root, files only root can read are skipped and the record is marked
incomplete.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return fmt.Errorf("carryon capture needs --out DIR for now: publishing into %s comes with the backup run (roadmap T11)", dirs.RecordDir())
			}
			if err := checkEmptyDir(out); err != nil {
				return err
			}
			return runCapture(cmd, dirs, out, !quick)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "empty folder to write the record into")
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
	run := sysexec.New()
	pm := pacman.New(run, pacman.Options{ExtraStandardRepos: cfg.Pacman.ExtraStandardRepos})
	if !pm.Present() {
		return errors.New("pacman isn't installed here: Carryon captures Arch-based systems for now")
	}
	users, warnings, err := capture.SelectUsers("/", cfg.Backup.Users)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		log.Warn("%s", w)
	}
	mounts, err := dest.ReadMounts("/")
	if err != nil {
		log.Warn("%v", err)
	}
	cat, err := catalog.Load(filepath.Join(dirs.Share, "catalog"))
	if err != nil {
		log.Warn("the app catalog wasn't read, so its excludes and hints are missing: %v", err)
	}
	tmp, err := os.MkdirTemp("", "carryon-capture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	excludes, warnings, err := capture.BuildExcludes(filepath.Join(tmp, "excludes"), capture.ExcludeSources{
		Dirs: dirs, Config: cfg, Catalog: cat, Users: users, Mounts: mounts,
	})
	if err != nil {
		return err
	}
	for _, w := range warnings {
		log.Warn("%s", w)
	}
	var hints []catalog.Hint
	if cat != nil {
		hints = cat.Hints()
	}
	res, err := capture.Run(cmd.Context(), capture.Options{
		Dir:      out,
		Runner:   run,
		Packages: pm,
		Config:   cfg,
		Users:    users,
		Excludes: excludes,
		Mounts:   mounts,
		Nightly:  nightly,
		Hints:    hints,
		IsRoot:   geteuid() == 0,
		Log:      log,
	})
	if err != nil {
		return err
	}
	printCaptureSummary(cmd.OutOrStdout(), res)
	return nil
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
