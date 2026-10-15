// Package cli defines Carryon's commands.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/ui"
	"github.com/Evimore/carryon/internal/version"
)

// Execute runs the command line and returns the process exit code.
func Execute() int {
	log := ui.Stderr()
	root := NewRoot(os.Stdout, paths.Default())
	if err := root.Execute(); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			return exit.code
		}
		log.Error("%v", err)
		return 1
	}
	return 0
}

// exitError ends the program with a code, after the command already reported why.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// NewRoot builds the command tree, writing normal output to out.
func NewRoot(out io.Writer, dirs paths.Dirs) *cobra.Command {
	root := &cobra.Command{
		Use:   "carryon",
		Short: "Back up what makes this machine yours, and get it back on any fresh install",
		Long: `Carryon records what makes this machine yours (packages, changed system files, services,
users and home folders), backs it up to a restic repository and rebuilds a fresh install from it,
all at once or one app at a time.

Carryon is in early development: commands marked "not implemented yet" are on the roadmap
(docs/roadmap.md in the source repository).`,
		Version:       version.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(out)
	root.SetVersionTemplate(version.String() + "\n")

	root.AddCommand(
		newVersionCmd(),
		newConfigCmd(dirs),
		newBundlesCmd(dirs),
		newAddCmd(dirs),
		newExcludeCmd(dirs),
		newResticCmd(dirs),
		newRcloneShimCmd(),
		newNotifyCmd(dirs),
	)
	for _, s := range plannedCommands {
		root.AddCommand(s.command())
	}
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return nil
		},
	}
}

// planned is a command that exists in the design but isn't built yet.
type planned struct {
	use, short, task string
	hidden           bool
}

var plannedCommands = []planned{
	{use: "init", short: "Set up backups on this machine, or connect it to an existing repository", task: "T17"},
	{use: "status", short: "Show recent and upcoming runs, and anything that needs attention", task: "T12"},
	{use: "backup", short: "Capture the system and back it up now", task: "T11"},
	{use: "capture", short: "Refresh the system record without backing up", task: "T07"},
	{use: "verify", short: "Check that the backup is current, complete and restorable", task: "T13"},
	{use: "restore", short: "Pick what to restore from a backup and apply it here", task: "T14 and T15"},
	{use: "inbox", short: "Review suggestions about new things to back up", task: "v0.2"},
	{use: "ignore", short: "Stop suggesting a path", task: "v0.2"},
	{use: "why", short: "Explain whether and how a path is backed up", task: "v0.2"},
	{use: "history", short: "Show changes to the system over time", task: "T09"},
	{use: "recovery-kit", short: "Show what you need to get back in on a new machine", task: "T17"},
	{use: "hook", short: "Called by the pacman hook", task: "T11", hidden: true},
	{use: "settle", short: "Back up once changes have settled", task: "T11", hidden: true},
	{use: "agent", short: "Run the per-user notification agent", task: "v0.2", hidden: true},
}

func (p planned) command() *cobra.Command {
	return &cobra.Command{
		Use:                p.use,
		Short:              p.short,
		Hidden:             p.hidden,
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("carryon %s is not implemented yet (roadmap: %s)", p.use, p.task)
		},
	}
}
