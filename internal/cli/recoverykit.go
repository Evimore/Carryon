package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/ui"
)

func newRecoveryKitCmd(dirs paths.Dirs) *cobra.Command {
	var saved bool
	cmd := &cobra.Command{
		Use:   "recovery-kit",
		Short: "Show what you need to get back in on a new machine",
		Long: `Shows the recovery kit: the repository, the machine name, the password, the steps to restore
on a new machine, and how to get files back with plain restic. It needs root, since it shows the
password. Keep it outside this machine, for example in your password manager.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if geteuid() != 0 {
				return errors.New("carryon recovery-kit shows the repository password, so it needs root: run it with sudo")
			}
			cfg, err := loadConfig(dirs)
			if err != nil {
				return err
			}
			env := setupEnv(dirs, newPrompter(false), cmd.OutOrStdout(), cmd.ErrOrStderr())
			err = env.RecoveryKit(cfg, saved)
			if errors.Is(err, ui.ErrCancelled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&saved, "saved", false, "note that you saved the kit, without asking")
	return cmd
}
