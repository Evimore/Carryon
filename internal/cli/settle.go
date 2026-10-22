package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
)

func newSettleCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "settle",
		Short: "Back up once changes have settled",
		Long: `Backs up when one is owed: after package changes, once nothing has changed for
backup.after_change_delay, and right away for a backup that waited for its destination.
carryon-settle.timer runs it every few minutes while ` + dirs.PendingFlag() + ` exists.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if geteuid() != 0 {
				return errors.New("carryon settle backs up the system, so it needs root")
			}
			log := logger(cmd.ErrOrStderr())
			p, ok, err := state.ReadPending(dirs)
			if err != nil {
				return err
			}
			if !ok {
				log.Info("No backup is owed.")
				return nil
			}
			if state.Restoring(dirs) {
				log.Info("Backups are paused while a restore is in progress.")
				return nil
			}
			ctx := cmd.Context()
			opt, err := backupOptions(ctx, cmd, dirs, log, true)
			if err != nil {
				return err
			}
			outcome, res, err := backup.Settle(ctx, opt)
			switch outcome {
			case backup.SettleNothing:
				if err == nil {
					log.Info("No backup is owed.")
				}
				return err
			case backup.SettleTooSoon:
				if backup.AfterChangeOff(opt.Config) {
					log.Info("Backups after package changes are off (backup.after_change_delay); the next scheduled backup includes them.")
				} else {
					log.Info("The last change was %s ago; the backup waits until nothing has changed for %s.",
						sinceText(opt.Now().Sub(p.Since)), opt.Config.Backup.AfterChangeDelay)
				}
				return nil
			case backup.SettleBusy:
				log.Info("Another backup is running; it covers the changes.")
				return nil
			}
			return reportBackup(cmd.OutOrStdout(), log, res, err, true)
		},
	}
}
