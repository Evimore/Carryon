package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
)

// historyGit runs git for carryon history; tests replace it.
var historyGit = func() sysexec.Runner { return sysexec.New() }

func newHistoryCmd(dirs paths.Dirs) *cobra.Command {
	var dir string
	var n int
	var changes, asJSON bool
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show changes to the system over time",
		Long: `Show how the system changed over time, newest first, from the system record's history.

Each capture that changed something (a package installed or removed, a setting file edited, a
service switched on...) adds an entry. --changes lists what changed in each.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			commits, err := record.History(cmd.Context(), dir, historyGit(), n)
			switch {
			case errors.Is(err, record.ErrNoHistory) && asJSON:
				commits, err = []record.Commit{}, nil
			case errors.Is(err, record.ErrNoHistory):
				fmt.Fprintln(cmd.OutOrStdout(), "No history yet: it starts with the first backup or capture.")
				return nil
			case errors.Is(err, record.ErrNotAllowed):
				return errors.New("the system record is only readable by root: run it with sudo (sudo carryon history)")
			case errors.Is(err, record.ErrNoGit):
				return errors.New("git isn't installed, so the system record has no history: install git " +
					"(sudo pacman -S git) and changes are recorded from the next backup on")
			case err != nil:
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if !changes {
					for i := range commits {
						commits[i].Changes = nil
					}
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				enc.SetEscapeHTML(false)
				return enc.Encode(commits)
			}
			for _, c := range commits {
				fmt.Fprintf(out, "%s  %s\n", c.Date.Format("2006-01-02 15:04"), c.Subject)
				if changes {
					for _, line := range c.Changes {
						fmt.Fprintf(out, "                  %s\n", line)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "number", "n", 10, "how many entries to show (0 for all)")
	cmd.Flags().BoolVar(&changes, "changes", false, "list what changed in each entry")
	cmd.Flags().StringVar(&dir, "record", dirs.RecordDir(), "record directory to read")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
