package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/setup"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

func newPasswordCmd(dirs paths.Dirs) *cobra.Command {
	var o setup.PasswordOptions
	cmd := &cobra.Command{
		Use:   "password",
		Short: "Change the repository password",
		Long: `Gives the repository a new password, for example when the old one leaked. Carryon makes a
strong password (or takes yours with --password-file), changes the repository's key to it, saves
it in /etc/carryon/repository-password, keeping the old one as repository-password.old, and shows
your new recovery kit. The old password stops opening the repository.

Other machines that back up to the same repository with the same password stop backing up until
the new password is saved there too: run "sudo carryon init" on each and enter it when asked.

With repository.password_command, Carryon changes the key and tells you to change what that
command reads.

This helps when only the password leaked. Whoever also had a copy of the repository's files from
before the change can still open it with the old password: then start a new repository instead.

Without a terminal, --yes changes the password and --kit-saved notes that you saved the new kit.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if geteuid() != 0 {
				return errors.New("carryon password changes the repository password and saves it where only root can read it, so it needs root: run it with sudo")
			}
			cfg, err := loadConfig(dirs)
			if err != nil {
				return err
			}
			env := setupEnv(dirs, newPrompter(cmd.Context(), false), cmd.OutOrStdout(), cmd.ErrOrStderr())
			env.RcloneRunner = rcloneRunner(cfg, sysexec.New())
			err = env.ChangePassword(cmd.Context(), cfg, o)
			if errors.Is(err, ui.ErrCancelled) {
				return errors.New("the password wasn't changed")
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.PasswordFile, "password-file", "", "use the password in this file instead of a generated one")
	f.BoolVar(&o.Yes, "yes", false, "change the password without asking")
	f.BoolVar(&o.KitSaved, "kit-saved", false, "you saved the new recovery kit, so don't ask")
	return cmd
}
