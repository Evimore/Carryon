// Package cli defines Carryon's commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/ui"
	"github.com/Evimore/carryon/internal/version"
)

// Execute runs the command line and returns the process exit code.
func Execute() int {
	log := ui.Stderr()
	ctx, stop := interruptible(context.Background())
	defer stop()
	root := NewRoot(os.Stdout, paths.Default())
	if err := root.ExecuteContext(ctx); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			return exit.code
		}
		log.Error("%v", err)
		return 1
	}
	return 0
}

// interruptible returns a context that Ctrl+C (SIGINT), SIGTERM and SIGHUP cancel. Commands then
// stop what they're doing and wait for restic to stop too, so it removes its lock: a carryon that
// exits at once leaves restic writing to pipes nobody reads, which kills it before it cleans up.
// After the first signal carryon stops listening, so a second one ends it at once, as by default.
func interruptible(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
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
		newInitCmd(dirs),
		newRecoveryKitCmd(dirs),
		newStatusCmd(dirs),
		newConfigCmd(dirs),
		newBundlesCmd(dirs),
		newCaptureCmd(dirs),
		newBackupCmd(dirs),
		newHookCmd(dirs),
		newSettleCmd(dirs),
		newVerifyCmd(dirs),
		newAddCmd(dirs),
		newExcludeCmd(dirs),
		newWhyCmd(dirs),
		newInboxCmd(dirs),
		newIgnoreCmd(dirs),
		newHistoryCmd(dirs),
		newRestoreCmd(dirs),
		newResticCmd(dirs),
		newRcloneShimCmd(),
		newNotifyCmd(dirs),
		newAgentCmd(dirs),
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

var plannedCommands = []planned{}

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
