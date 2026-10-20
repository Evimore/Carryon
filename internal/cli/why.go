package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
	"github.com/Evimore/carryon/internal/why"
)

func newWhyCmd(dirs paths.Dirs) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "why PATH",
		Short: "Explain whether and how a path is backed up",
		Long: `Say in plain words whether a path is in the backup and why:

  - the backed-up folder it is in (/etc, /root, /srv, backup.extra_paths, a user's home);
  - the exclude pattern that leaves it out and where the pattern comes from (the defaults,
    the app catalog, excludes.txt, a mount, the backup's own folder), or a ! pattern that
    takes it back in;
  - another filesystem or mount it is on, which the backup doesn't go into, and how to take
    it in;
  - the item in the system record that holds it, its bundle, the rule that put it there, the
    bundle's when-rule and whether it starts ticked when restoring here;
  - a package file you haven't changed, which comes back with its package, or data that
    downloads itself again;
  - a folder in detect.ignore, or a suggestion waiting in carryon inbox.

PATH doesn't have to exist. The system record can only be read as root: without sudo, carryon why
says what the settings tell.

--json prints the answer for scripts.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("can't make %q a full path: %w", args[0], err)
			}
			in, err := whyInput(cmd, dirs, p)
			if err != nil {
				return err
			}
			a := why.Explain(cmd.Context(), in)
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				enc.SetEscapeHTML(false)
				return enc.Encode(a)
			}
			return why.WriteText(out, a)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// whyInput reads what carryon why needs from the machine and the settings. Without root, the
// record and some users' settings can't be read; the answer says so.
func whyInput(cmd *cobra.Command, dirs paths.Dirs, p string) (why.Input, error) {
	ctx := cmd.Context()
	log := ui.Stderr()
	cfg, _, err := config.Load(dirs.ConfigFile())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfg = config.Default()
	case err != nil:
		return why.Input{}, err
	}
	sys, err := backupSystem(cfg)
	if err != nil {
		// Without a package manager, Carryon can't tell which package owns the path.
		sys = backup.System{Root: "/"}
	}
	root := sys.Root
	if root == "" {
		root = "/"
	}
	in := why.Input{Path: p, Root: root, RecordDir: dirs.RecordDir(), Ignored: cfg.Detect.Ignore}
	if inbox, err := state.ReadInbox(dirs); err == nil {
		in.Inbox = inbox.Entries
	}

	rec, err := record.Load(dirs.RecordDir())
	switch {
	case err == nil:
		in.Record = rec
	case geteuid() != 0:
	case errors.Is(err, fs.ErrNotExist):
		in.RecordError = "there is none yet: sudo carryon capture writes it"
	default:
		in.RecordError = err.Error()
	}

	users := sys.Users
	if users == nil {
		if users, _, err = capture.SelectUsers(root, cfg.Backup.Users); err != nil {
			return why.Input{}, err
		}
	}
	if everyone, _, err := capture.SelectUsers(root, []string{"*"}); err == nil {
		in.OtherUsers = map[string]string{}
		for _, u := range everyone {
			in.OtherUsers[u.Name] = u.Home
		}
		for _, u := range users {
			delete(in.OtherUsers, u.Name)
		}
	}

	mounts := sys.Mounts
	if mounts == nil {
		if mounts, err = dest.ReadMounts(root); err != nil {
			log.Warn("%v", err)
		}
	}
	cat := sys.Catalog
	if cat == nil {
		if cat, err = catalog.Load(filepath.Join(dirs.Share, "catalog")); err != nil {
			log.Warn("the app catalog wasn't read, so its excludes are missing here: %v", err)
		}
	}
	excludes, _, err := capture.ExcludeInputs(capture.ExcludeSources{
		Dirs: dirs, Config: cfg, Catalog: cat, Users: users, Mounts: backup.WithoutSubvolumes(mounts),
	})
	if err != nil {
		if geteuid() != 0 {
			return why.Input{}, fmt.Errorf("%w\nRun it with sudo to read every setting: sudo carryon why %s", err, p)
		}
		return why.Input{}, err
	}
	in.Excludes = excludes.List(true)
	if cat != nil {
		in.Hints = cat.Hints()
	}

	var items []record.Item
	if in.Record != nil {
		items = in.Record.Items
	}
	in.Targets = backup.Targets(sys, cfg, items, users)
	in.Crossing = func(target, path string) backup.Crossing { return backup.CrossingTo(sys, mounts, target, path) }

	if f, err := os.Open(dirs.BundlesFile()); err == nil {
		in.Rules, _, _ = rules.ParseBundleRules(f, dirs.BundlesFile())
		f.Close()
	}
	run := sys.Runner
	if run == nil {
		run = sysexec.New()
	}
	if here, err := facts.New(root, run).Facts(ctx); err == nil {
		in.Here = &here
	}
	if sys.Packages != nil {
		in.Machine = capture.NewBundleMachine(root, run, sys.Packages, nil)
		if owners, err := sys.Packages.Owners(ctx, []string{p}); err == nil {
			in.Owner = owners[p]
		}
	}
	return in, nil
}
