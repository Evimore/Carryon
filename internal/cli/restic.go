package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/ui"
)

func newResticCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "restic ARGS...",
		Short: "Run a restic command on the repository",
		Long: `Runs restic with the repository, password and rclone login from Carryon's config,
for example: sudo carryon restic snapshots. Everything after "restic" goes to restic unchanged,
and Carryon exits with restic's exit code.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if geteuid() != 0 {
				return errors.New("carryon restic needs root to read the repository password: run it with sudo")
			}
			cfg, err := loadConfig(dirs)
			if err != nil {
				return err
			}
			e := engine.New(cfg.Repository, dirs, cfg.MachineName)
			c, err := e.Command(args...)
			if err != nil {
				return err
			}
			c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()

			// Ctrl-C reaches restic straight from the terminal; carryon waits for restic to finish.
			// A SIGTERM sent to carryon alone is passed on as SIGINT, so restic removes its lock.
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			sigs := make(chan os.Signal, 1)
			signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer signal.Stop(sigs)
			go func() {
				for s := range sigs {
					if s != os.Interrupt {
						cancel()
					}
				}
			}()

			code, err := engine.ExecRunner(ctx, c)
			if err != nil {
				return fmt.Errorf("restic couldn't be started: %w\n   install restic %s or newer (on Arch: sudo pacman -S restic)", err, engine.MinVersion)
			}
			if code != 0 {
				return exitError{code: code}
			}
			return nil
		},
	}
}

// loadConfig reads and checks the config, for commands that need a repository.
func loadConfig(dirs paths.Dirs) (config.Config, error) {
	file := dirs.ConfigFile()
	cfg, warnings, err := config.Load(file)
	if errors.Is(err, fs.ErrNotExist) {
		return config.Config{}, fmt.Errorf("%s doesn't exist: set up backups first with: sudo carryon init", file)
	}
	if err != nil {
		return config.Config{}, err
	}
	for _, w := range warnings {
		ui.Stderr().Warn("%s", w)
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("%s has problems:\n%w", file, err)
	}
	if cfg.Repository.URL == "" {
		return config.Config{}, fmt.Errorf("%s sets no repository.url: set up backups first with: sudo carryon init", file)
	}
	return cfg, nil
}

func newRcloneShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "rclone-shim ARGS...",
		Short:              "Run rclone as the configured user (restic starts this)",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			// Only returns on failure; stdout belongs to restic, so errors go to stderr.
			if err := engine.RunShim(args); err != nil {
				return fmt.Errorf("rclone-shim: %w", err)
			}
			return nil
		},
	}
}
