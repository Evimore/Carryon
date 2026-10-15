package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

func newConfigCmd(dirs paths.Dirs) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and check settings",
	}
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
	cmd.AddCommand(check)
	return cmd
}
