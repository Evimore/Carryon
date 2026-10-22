package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/verify"
)

// verifyUnit is the service the verify timer starts.
const verifyUnit = "carryon-verify.service"

func newVerifyCmd(dirs paths.Dirs) *cobra.Command {
	var deep, full, asJSON bool
	cmd := &cobra.Command{
		Use:   "verify [--deep|--full]",
		Short: "Check that the backup is current, complete and restorable",
		Long: `Checks the system record (complete, recent, matching this machine), that everything in it
can be installed again (repository and AUR packages, extra repositories and their keys), that what
this machine runs is backed up (programs started by your own units, DKMS drivers, kernel
parameters, mounts, kernel modules), and the repository: it opens, the newest backup is recent
and the recovery kit is saved.

--deep also looks inside the newest backup, re-reads part of the data (verify.read_subset) and
restores a few files into a temporary folder. --full re-reads all of the data, which downloads
the whole repository.

Nothing runs on the repository unless it is the one set up with carryon init, and verify writes
nothing to it except the lock restic check needs. It exits with status 1 when it finds a problem;
warnings alone don't fail.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deep && full {
				return errors.New("--deep and --full don't go together: --full already includes everything --deep checks")
			}
			if geteuid() != 0 {
				return errors.New("carryon verify reads the system record and the repository password, so it needs root: run it with sudo")
			}
			log := logger(cmd.ErrOrStderr())
			ctx := cmd.Context()
			bopt, err := backupOptions(ctx, cmd, dirs, log, true)
			if err != nil {
				return err
			}
			trigger := state.TriggerManual
			if bopt.Unit == verifyUnit {
				trigger = state.TriggerTimer
			}
			rep, err := verify.Run(ctx, verify.Options{
				Dirs:       dirs,
				Config:     bopt.Config,
				System:     bopt.System,
				Engine:     bopt.Engine,
				Checker:    bopt.Checker,
				Machine:    bopt.Machine,
				Deep:       deep,
				Full:       full,
				Trigger:    trigger,
				Unit:       bopt.Unit,
				Invocation: bopt.Invocation,
				Redact:     bopt.Redact,
				Timers:     bopt.Timers,
				Log:        log,
				Now:        bopt.Now,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if err := rep.WriteJSON(out); err != nil {
					return err
				}
			} else {
				rep.Write(out, out == os.Stdout && isTerminal(os.Stdout))
			}
			if !rep.OK() {
				return exitError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "also look inside the newest backup, re-read part of the data and try a restore")
	cmd.Flags().BoolVar(&full, "full", false, "like --deep, but re-read all of the data")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
