package cli

import (
	"context"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/setup"
	"github.com/Evimore/carryon/internal/ui"
)

// newPrompter returns how commands ask the user things; tests replace it with a ui.Script. The
// questions end with ctx, so Ctrl+C ends them.
var newPrompter = func(ctx context.Context, defaults bool) ui.Prompter {
	return ui.New(ui.Options{Defaults: defaults, Context: ctx})
}

// setupEnv returns what init and recovery-kit use from the machine; tests replace it to use temp
// folders, a fake mount table and a fake systemctl.
var setupEnv = func(dirs paths.Dirs, p ui.Prompter, out, errOut io.Writer) *setup.Env {
	return &setup.Env{Dirs: dirs, Prompt: p, Log: logger(errOut), Out: out}
}

func newInitCmd(dirs paths.Dirs) *cobra.Command {
	var (
		o        setup.Options
		connect  string
		defaults bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up backups on this machine, or connect it to an existing repository",
		Long: `Sets up backups on this machine. It asks where backups go, for the password, the machine
name, whose home folders to back up and when. Then it creates the encrypted repository (or opens
the one already there), saves the settings in /etc/carryon, switches on the timers and shows
your recovery kit. Running it again keeps your settings and lets you change them.

Every question has a flag, so init also runs without a terminal: --defaults takes the default
answer for everything not given as a flag.

On a new machine, connect to your existing repository and restore from it:
    sudo carryon init --connect sftp:alice@example.com:/backups
    sudo carryon restore

Or restore as you go: backups start at once, and installing an app offers its saved settings:
    sudo carryon init --connect sftp:alice@example.com:/backups --restore as-you-go`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := needRoot("carryon init"); err != nil {
				return err
			}
			if connect != "" && o.Repo != "" {
				return errors.New("--repo is for setting up a new destination; --connect brings its own")
			}
			if o.SettingsFrom != "" && connect == "" {
				return errors.New("--settings-from only works with --connect")
			}
			if o.Restore != "" && connect == "" {
				return errors.New("--restore only works with --connect")
			}
			if o.PasswordFile != "" && o.PasswordCommand != "" {
				return errors.New("use --password-file or --password-command, not both")
			}
			env := setupEnv(dirs, newPrompter(cmd.Context(), defaults), cmd.OutOrStdout(), cmd.ErrOrStderr())
			ctx := cmd.Context()
			var err error
			switch {
			case connect != "":
				err = env.Connect(ctx, connect, o)
			default:
				err = env.Init(ctx, o)
			}
			if errors.Is(err, ui.ErrCancelled) {
				return errors.New("init was cancelled")
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Repo, "repo", "", "where backups go: a folder or any restic repository URL")
	f.StringVar(&o.PasswordFile, "password-file", "", "read the repository password from this file")
	f.StringVar(&o.PasswordCommand, "password-command", "", "get the repository password from this command, such as systemd-creds")
	f.StringVar(&o.MachineName, "machine-name", "", "the name this machine's backups are kept under (default: the hostname)")
	f.StringSliceVar(&o.Users, "users", nil, `whose home folders to back up: "*" for everyone, or names separated by commas`)
	f.StringVar(&o.Schedule, "schedule", "", `when the regular backup runs, as a systemd calendar time (default "daily")`)
	f.StringVar(&o.VerifySchedule, "verify-schedule", "", `when the deep check runs (default "weekly")`)
	f.StringVar(&o.RcloneUser, "rclone-user", "", "for rclone: repositories, the user whose rclone login to use")
	f.StringVar(&connect, "connect", "", "connect this machine to the existing repository at this URL, to restore from it")
	f.StringVar(&o.SettingsFrom, "settings-from", "", `with --connect: reuse this machine's settings, or "none"`)
	f.StringVar(&o.Restore, "restore", "", `with --connect: restore everything "now" (backups pause until it's done), or "as-you-go" (default "now")`)
	f.BoolVar(&defaults, "defaults", false, "take the default answer for every question not given as a flag")
	f.BoolVar(&o.Yes, "yes", false, "go ahead with a risky destination (system disk, network mount) without asking")
	f.BoolVar(&o.KitSaved, "kit-saved", false, "you saved the recovery kit, so don't ask")
	return cmd
}
