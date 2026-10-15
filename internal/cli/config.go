package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

// geteuid is the effective user ID; tests replace it to run commands that need root.
var geteuid = os.Geteuid

// needRoot refuses to go on unless Carryon runs as root.
func needRoot(command string) error {
	if geteuid() != 0 {
		return fmt.Errorf("%s changes the system settings, so it needs root: run it with sudo", command)
	}
	return nil
}

func newConfigCmd(dirs paths.Dirs) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show, check and change settings",
	}
	cmd.AddCommand(newConfigCheckCmd(dirs), newConfigGetCmd(dirs), newConfigSetCmd(dirs))
	return cmd
}

func newConfigCheckCmd(dirs paths.Dirs) *cobra.Command {
	var file string
	check := &cobra.Command{
		Use:   "check",
		Short: "Check the config file for mistakes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			cfg, warnings, err := config.Load(file)
			if err != nil {
				return err
			}
			for _, w := range warnings {
				fmt.Fprintln(out, "warning:", w)
			}
			if err := cfg.Validate(); err != nil {
				fmt.Fprintf(out, "%s has problems:\n%v\n", file, err)
				return exitError{code: 1}
			}
			fmt.Fprintf(out, "%s is valid\n", file)
			return nil
		},
	}
	check.Flags().StringVar(&file, "file", dirs.ConfigFile(), "config file to check")
	return check
}

func newConfigGetCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "get [KEY]",
		Short: "Show one setting, or all of them",
		Long: `Show one setting, such as backup.after_change_delay or retention.keep_daily, or all of
them when no setting is named. Settings missing from the file show their default.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			cfg, _, err := config.Load(dirs.ConfigFile())
			if errors.Is(err, fs.ErrNotExist) {
				cfg = config.Default()
			} else if err != nil {
				return err
			}
			if len(args) == 0 {
				for _, k := range config.Keys() {
					v, _ := cfg.Get(k)
					fmt.Fprintf(out, "%s = %s\n", k, v)
				}
				return nil
			}
			v, err := cfg.Get(args[0])
			if err != nil {
				return keyHint(err)
			}
			fmt.Fprintln(out, v)
			return nil
		},
	}
}

func newConfigSetCmd(dirs paths.Dirs) *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Change a setting",
		Long: `Change a setting, such as backup.after_change_delay or retention.keep_daily.

Lists take comma-separated items or a TOML array:
  carryon config set backup.extra_paths "/root, /srv"
  carryon config set backup.extra_paths '["/root", "/srv"]'
An empty value makes an empty list.

The whole config is checked before anything is saved. Carryon writes the file in its own
layout, so comments you added to it are not kept.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := needRoot("carryon config set"); err != nil {
				return err
			}
			key, value := args[0], args[1]
			var before, after string
			notes, err := config.Update(dirs.ConfigFile(), func(c *config.Config) error {
				var err error
				if before, err = c.Get(key); err != nil {
					return keyHint(err)
				}
				if err := c.Set(key, value); err != nil {
					return err
				}
				after, _ = c.Get(key)
				return nil
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, n := range notes {
				fmt.Fprintln(out, "note:", n)
			}
			if before == after {
				fmt.Fprintf(out, "%s is already %s\n", key, show(after))
				return nil
			}
			fmt.Fprintf(out, "%s: %s -> %s (in %s)\n", key, show(before), show(after), dirs.ConfigFile())
			return nil
		},
	}
}

// keyHint adds where to find the setting names to an unknown-setting error.
func keyHint(err error) error {
	if strings.Contains(err.Error(), "unknown setting") {
		return fmt.Errorf("%w; \"carryon config get\" lists them all", err)
	}
	return err
}

// show makes an empty value visible in messages.
func show(v string) string {
	if v == "" {
		return `""`
	}
	return v
}
